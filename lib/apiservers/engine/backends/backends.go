// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backends

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/registry"
	httptransport "github.com/go-swagger/go-swagger/httpkit/client"
	"github.com/go-swagger/go-swagger/swag"

	"github.com/vmware/vic/lib/apiservers/engine/backends/cache"
	"github.com/vmware/vic/lib/apiservers/engine/backends/container"
	"github.com/vmware/vic/lib/apiservers/engine/backends/portmap"
	"github.com/vmware/vic/lib/apiservers/portlayer/client"
	"github.com/vmware/vic/lib/apiservers/portlayer/client/containers"
	"github.com/vmware/vic/lib/apiservers/portlayer/client/misc"
	"github.com/vmware/vic/lib/apiservers/portlayer/client/scopes"
	"github.com/vmware/vic/lib/apiservers/portlayer/client/storage"
	"github.com/vmware/vic/lib/apiservers/portlayer/models"
	"github.com/vmware/vic/lib/config"
	"github.com/vmware/vic/pkg/errors"
	"github.com/vmware/vic/pkg/vsphere/sys"
)

const (
	Imagec             = "imagec"
	PortlayerName      = "Backend Engine"
	IndexServerAddress = "registry-1.docker.io"

	// RetryTimeSeconds defines how many seconds to wait between retries
	RetryTimeSeconds = 2
)

var (
	portLayerClient     *client.PortLayer
	portLayerServerAddr string
	portLayerName       string
	productName         string
	productVersion      string

	vchConfig *config.VirtualContainerHostConfigSpec

	insecureRegistries []string
	RegistryService    *registry.Service
)

func Init(portLayerAddr, product string, config *config.VirtualContainerHostConfigSpec, insecureRegs []url.URL) error {
	_, _, err := net.SplitHostPort(portLayerAddr)
	if err != nil {
		return err
	}

	vchConfig = config
	productName = product

	if config != nil {
		if config.Version != nil {
			productVersion = config.Version.ShortVersion()
		}
		if productVersion == "" {
			portLayerName = product + " Backend Engine"
		} else {
			portLayerName = product + " " + productVersion + " Backend Engine"
		}
	} else {
		portLayerName = product + " Backend Engine"
	}

	t := httptransport.New(portLayerAddr, "/", []string{"http"})
	portLayerClient = client.New(t, nil)
	portLayerServerAddr = portLayerAddr

	// block indefinitely while waiting on the portlayer to respond to pings
	// the vic-machine installer timeout will intervene if this blocks for too long
	pingPortLayer()

	log.Info("Creating image store")
	if err := createImageStore(); err != nil {
		log.Errorf("Failed to create image store")
		return err
	}

	log.Info("Refreshing image cache")
	go func() {
		if err := cache.ImageCache().Update(portLayerClient); err != nil {
			log.Warnf("Failed to refresh image cache: %s", err)
			return
		}
		log.Info("Image cache updated successfully")
	}()

	log.Info("Refreshing container cache")
	go func() {
		if err := syncContainerCache(); err != nil {
			log.Warnf("Failed to refresh container cache: %s", err)
			return
		}
		log.Info("Container cache updated successfully")
	}()

	// creates and potentially restore repository cache
	if err := cache.NewRepositoryCache(portLayerClient); err != nil {
		return fmt.Errorf("Failed to create repository cache: %s", err.Error())
	}

	serviceOptions := registry.ServiceOptions{}
	for _, registry := range insecureRegs {
		insecureRegistries = append(insecureRegistries, registry.Path)
	}
	if len(insecureRegistries) > 0 {
		serviceOptions.InsecureRegistries = insecureRegistries
	}
	log.Debugf("New registry service with options %#v", serviceOptions)
	RegistryService = registry.NewService(serviceOptions)

	return nil
}

func PortLayerClient() *client.PortLayer {
	return portLayerClient
}

func PortLayerServer() string {
	return portLayerServerAddr
}

func PortLayerName() string {
	return portLayerName
}

func ProductName() string {
	return productName
}

func ProductVersion() string {
	return productVersion
}

func VchConfig() *config.VirtualContainerHostConfigSpec {
	return vchConfig
}

func pingPortLayer() {
	ticker := time.NewTicker(RetryTimeSeconds * time.Second)
	defer ticker.Stop()
	params := misc.NewPingParamsWithContext(context.TODO())

	log.Infof("Waiting for portlayer to come up")

	for range ticker.C {
		if _, err := portLayerClient.Misc.Ping(params); err == nil {
			log.Info("Portlayer is up and responding to pings")
			return
		}
	}
}

func createImageStore() error {
	// TODO(jzt): we should move this to a utility package or something
	host, err := sys.UUID()
	if err != nil {
		log.Errorf("Failed to determine host UUID")
		return err
	}

	// attempt to create the image store if it doesn't exist
	store := &models.ImageStore{Name: host}
	_, err = portLayerClient.Storage.CreateImageStore(
		storage.NewCreateImageStoreParamsWithContext(ctx).WithBody(store),
	)

	if err != nil {
		if _, ok := err.(*storage.CreateImageStoreConflict); ok {
			log.Debugf("Store already exists")
			return nil
		}
		return err
	}
	log.Infof("Image store created successfully")
	return nil
}

func InsecureRegistries() []string {
	registries := make([]string, len(insecureRegistries))
	for _, reg := range insecureRegistries {
		registries = append(registries, reg)
	}

	return registries
}

// syncContainerCache runs once at startup to populate the container cache
func syncContainerCache() error {
	log.Debugf("Sync up container cache from portlyaer")

	backend := NewContainerBackend()
	client := backend.containerProxy.Client()

	reqParams := containers.NewGetContainerListParamsWithContext(ctx).WithAll(swag.Bool(true))
	containme, err := client.Containers.GetContainerList(reqParams)
	if err != nil {
		return errors.Errorf("Failed to retrieve container list from portlayer: %s", err)
	}

	log.Debugf("Found %d containers", len(containme.Payload))
	cc := cache.ContainerCache()
	var errs []string
	for _, info := range containme.Payload {
		container := ContainerInfoToVicContainer(*info)
		cc.AddContainer(container)
		if err = setPortMapping(info, backend, container); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.Errorf("Failed to set port mapping: %s", strings.Join(errs, "\n"))
	}
	return nil
}

func setPortMapping(info *models.ContainerInfo, backend *Container, container *container.VicContainer) error {
	if info.ContainerConfig.State == nil {
		log.Infof("container state is nil")
		return nil
	}
	if *info.ContainerConfig.State != "Running" || len(container.HostConfig.PortBindings) == 0 {
		log.Infof("Container info state: %s", *info.ContainerConfig.State)
		log.Infof("container portbinding: %+v", container.HostConfig.PortBindings)
		return nil
	}

	log.Debugf("Set port mapping for container %q, portmapping %+v", container.Name, container.HostConfig.PortBindings)
	client := backend.containerProxy.Client()
	endpointsOK, err := client.Scopes.GetContainerEndpoints(
		scopes.NewGetContainerEndpointsParamsWithContext(ctx).WithHandleOrID(container.ContainerID))
	if err != nil {
		return err
	}
	for _, e := range endpointsOK.Payload {
		if len(e.Ports) > 0 {
			if err = backend.mapPorts(portmap.Map, container.HostConfig, e); err != nil {
				return err
			}
		}
	}
	return nil
}
