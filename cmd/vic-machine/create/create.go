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

package create

import (
	"bytes"
	"crypto/tls"
	"encoding"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/urfave/cli"

	"github.com/vmware/vic/lib/install/data"
	"github.com/vmware/vic/lib/install/management"
	"github.com/vmware/vic/lib/install/validate"
	"github.com/vmware/vic/pkg/certificate"
	"github.com/vmware/vic/pkg/errors"
	"github.com/vmware/vic/pkg/flags"
	"github.com/vmware/vic/pkg/ip"
	"github.com/vmware/vic/pkg/trace"

	"golang.org/x/net/context"
)

const (
	// Max permitted length of Virtual Machine name
	MaxVirtualMachineNameLen = 80
	// Max permitted length of Virtual Switch name
	MaxDisplayNameLen = 31
)

var EntireOptionHelpTemplate = `NAME:
   {{.HelpName}} - {{.Usage}}

USAGE:
   {{.HelpName}}{{if .VisibleFlags}} [command options]{{end}} {{if .ArgsUsage}}{{.ArgsUsage}}{{else}}[arguments...]{{end}}{{if .Category}}

CATEGORY:
   {{.Category}}{{end}}{{if .Description}}

DESCRIPTION:
   {{.Description}}{{end}}{{if .VisibleFlags}}

OPTIONS:
   {{range .Flags}}{{.}}
   {{end}}{{end}}
`

// Create has all input parameters for vic-machine create command
type Create struct {
	*data.Data

	cert       string
	key        string
	cacert     string
	cakey      string
	clientCert *tls.Certificate

	envFile string

	cname   string
	org     cli.StringSlice
	keySize int

	noTLS           bool
	noTLSverify     bool
	advancedOptions bool

	clientCAs cli.StringSlice

	containerNetworks         cli.StringSlice
	containerNetworksGateway  cli.StringSlice
	containerNetworksIPRanges cli.StringSlice
	containerNetworksDNS      cli.StringSlice
	volumeStores              cli.StringSlice
	insecureRegistries        cli.StringSlice
	dns                       cli.StringSlice
	clientNetworkName         string
	clientNetworkGateway      string
	clientNetworkIP           string
	externalNetworkName       string
	externalNetworkGateway    string
	externalNetworkIP         string
	managementNetworkName     string
	managementNetworkGateway  string
	managementNetworkIP       string

	memoryReservLimits string
	cpuReservLimits    string

	BridgeIPRange string

	executor *management.Dispatcher
}

func NewCreate() *Create {
	create := &Create{}
	create.Data = data.NewData()

	return create
}

// Flags return all cli flags for create
func (c *Create) Flags() []cli.Flag {
	create := []cli.Flag{
		// images
		cli.StringFlag{
			Name:        "image-store, i",
			Value:       "",
			Usage:       "Image datastore path in format \"datastore/path\"",
			Destination: &c.ImageDatastorePath,
		},
		cli.StringFlag{
			Name:        "base-image-size",
			Value:       "8GB",
			Usage:       "Specify the size of the base image from which all other images are created e.g. 8GB/8000MB",
			Destination: &c.ScratchSize,
			Hidden:      true,
		},

		// container disk
		cli.StringFlag{
			Name:        "container-store, cs",
			Value:       "",
			Usage:       "Container datastore path - not supported yet, defaults to image datastore",
			Destination: &c.ContainerDatastoreName,
			Hidden:      true,
		},

		// volume
		cli.StringSliceFlag{
			Name:  "volume-store, vs",
			Value: &c.volumeStores,
			Usage: "Specify a list of location and label for volume store, e.g. \"datastore/path:label\" or \"datastore:label\".",
		},

		// bridge
		cli.StringFlag{
			Name:        "bridge-network, b",
			Value:       "",
			Usage:       "The bridge network port group name (private port group for containers). Defaults to the Virtual Container Host name",
			Destination: &c.BridgeNetworkName,
		},
		cli.StringFlag{
			Name:        "bridge-network-range, bnr",
			Value:       "172.16.0.0/12",
			Usage:       "The IP range from which bridge networks are allocated",
			Destination: &c.BridgeIPRange,
			Hidden:      true,
		},

		// client
		cli.StringFlag{
			Name:        "client-network, cln",
			Value:       "",
			Usage:       "The client network port group name (restricts DOCKER_API access to this network). Defaults to DCHP - see advanced help (-x)",
			Destination: &c.clientNetworkName,
		},
		cli.StringFlag{
			Name:        "client-network-gateway",
			Value:       "",
			Usage:       "Gateway for the VCH on the client network, e.g. 10.0.0.1/24",
			Destination: &c.clientNetworkGateway,
			Hidden:      true,
		},
		cli.StringFlag{
			Name:        "client-network-ip",
			Value:       "",
			Usage:       "IP address for the VCH on the client network, e.g. 10.0.0.2/24",
			Destination: &c.clientNetworkIP,
			Hidden:      true,
		},

		// external
		cli.StringFlag{
			Name:        "external-network, en",
			Value:       "",
			Usage:       "The external network port group name (port forwarding and default route). Defaults to 'VM Network' and DHCP -- see advanced help (-x)",
			Destination: &c.externalNetworkName,
		},
		cli.StringFlag{
			Name:        "external-network-gateway",
			Value:       "",
			Usage:       "Gateway for the VCH on the external network, e.g. 10.0.1.1/24",
			Destination: &c.externalNetworkGateway,
			Hidden:      true,
		},
		cli.StringFlag{
			Name:        "external-network-ip",
			Value:       "",
			Usage:       "IP address for the VCH on the external network, e.g. 10.0.1.2/24",
			Destination: &c.externalNetworkIP,
			Hidden:      true,
		},

		// management
		cli.StringFlag{
			Name:        "management-network, mn",
			Value:       "",
			Usage:       "The management network port group name (provides route to target hosting vSphere). Defaults to DCHP - see advanced help (-x)",
			Destination: &c.managementNetworkName,
		},
		cli.StringFlag{
			Name:        "management-network-gateway",
			Value:       "",
			Usage:       "Gateway for the VCH on the management network, e.g. 10.0.2.1/24",
			Destination: &c.managementNetworkGateway,
			Hidden:      true,
		},
		cli.StringFlag{
			Name:        "management-network-ip",
			Value:       "",
			Usage:       "IP address for the VCH on the management network, e.g. 10.0.2.2/24",
			Destination: &c.managementNetworkIP,
			Hidden:      true,
		},

		// general DNS
		cli.StringSliceFlag{
			Name:   "dns-server",
			Value:  &c.dns,
			Usage:  "DNS server for the client, external, and management networks. Defaults to 8.8.8.8 and 8.8.4.4 when not using DHCP",
			Hidden: true,
		},

		// container networks - mapped from vSphere
		cli.StringSliceFlag{
			Name:  "container-network, cn",
			Value: &c.containerNetworks,
			Usage: "vSphere network list that containers can use directly with labels, e.g. vsphere-net:backend. Defaults to DCHP - see advanced help (-x).",
		},
		cli.StringSliceFlag{
			Name:   "container-network-gateway, cng",
			Value:  &c.containerNetworksGateway,
			Usage:  "Gateway for the container network's subnet in CONTAINER-NETWORK:SUBNET format, e.g. vsphere-net:172.16.0.0/16.",
			Hidden: true,
		},
		cli.StringSliceFlag{
			Name:   "container-network-ip-range, cnr",
			Value:  &c.containerNetworksIPRanges,
			Usage:  "IP range for the container network in CONTAINER-NETWORK:IP-RANGE format, e.g. vsphere-net:172.16.0.0/24, vsphere-net:172.16.0.10-20.",
			Hidden: true,
		},
		cli.StringSliceFlag{
			Name:   "container-network-dns, cnd",
			Value:  &c.containerNetworksDNS,
			Usage:  "DNS servers for the container network in CONTAINER-NETWORK:DNS format, e.g. vsphere-net:8.8.8.8. Ignored if no static IP assigned.",
			Hidden: true,
		},

		// memory
		cli.IntFlag{
			Name:        "memory, mem",
			Value:       0,
			Usage:       "VCH resource pool memory limit in MB (unlimited=0)",
			Destination: &c.VCHMemoryLimitsMB,
		},
		cli.IntFlag{
			Name:        "memory-reservation, memr",
			Value:       0,
			Usage:       "VCH resource pool memory reservation in MB",
			Destination: &c.VCHMemoryReservationsMB,
			Hidden:      true,
		},
		cli.GenericFlag{
			Name:   "memory-shares, mems",
			Value:  flags.NewSharesFlag(&c.VCHMemoryShares),
			Usage:  "VCH resource pool memory shares in level or share number, e.g. high, normal, low, or 163840",
			Hidden: true,
		},
		cli.IntFlag{
			Name:        "appliance-memory",
			Value:       2048,
			Usage:       "Memory for the appliance VM, in MB. Does not impact resources allocated per container.",
			Hidden:      true,
			Destination: &c.MemoryMB,
		},

		// cpu
		cli.IntFlag{
			Name:        "cpu",
			Value:       0,
			Usage:       "VCH resource pool vCPUs limit in MHz (unlimited=0)",
			Destination: &c.VCHCPULimitsMHz,
		},
		cli.IntFlag{
			Name:        "cpu-reservation, cpur",
			Value:       0,
			Usage:       "VCH resource pool reservation in MHz",
			Destination: &c.VCHCPUReservationsMHz,
			Hidden:      true,
		},
		cli.GenericFlag{
			Name:   "cpu-shares, cpus",
			Value:  flags.NewSharesFlag(&c.VCHCPUShares),
			Usage:  "VCH VCH resource pool vCPUs shares, in level or share number, e.g. high, normal, low, or 4000",
			Hidden: true,
		},
		cli.IntFlag{
			Name:        "appliance-cpu",
			Value:       1,
			Usage:       "vCPUs for the appliance VM",
			Hidden:      true,
			Destination: &c.NumCPUs,
		},

		// TLS
		cli.StringFlag{
			Name:        "tls-cname",
			Value:       "",
			Usage:       "Common Name to use in generated CA certificate when requiring client certificate authentication",
			Destination: &c.cname,
		},
		cli.StringSliceFlag{
			Name:   "organization",
			Usage:  "A list of identifiers to record in the generated certificates. Defaults to VCH name and IP/FQND if provided.",
			Value:  &c.org,
			Hidden: true,
		},
		cli.BoolFlag{
			Name:        "no-tlsverify, kv",
			Usage:       "Disable authentication via client certificates - for more tls options see advanced help (-x)",
			Destination: &c.noTLSverify,
		},
		cli.BoolFlag{
			Name:        "no-tls, k",
			Usage:       "Disable TLS support completely",
			Destination: &c.noTLS,
			Hidden:      true,
		},
		cli.StringFlag{
			Name:        "key",
			Value:       "",
			Usage:       "Virtual Container Host private key file",
			Destination: &c.key,
			Hidden:      true,
		},
		cli.StringFlag{
			Name:        "cert",
			Value:       "",
			Usage:       "Virtual Container Host x509 certificate file",
			Destination: &c.cert,
			Hidden:      true,
		},
		cli.StringSliceFlag{
			Name:   "tls-ca, ca",
			Usage:  "Specify a list of certificate authority files to use for client verification",
			Value:  &c.clientCAs,
			Hidden: true,
		},
		cli.IntFlag{
			Name:        "certificate-key-size, ksz",
			Usage:       "Size of key to use when generating certificates",
			Value:       2048,
			Destination: &c.keySize,
			Hidden:      true,
		},

		// registries
		cli.StringSliceFlag{
			Name:  "insecure-registry, dir",
			Value: &c.insecureRegistries,
			Usage: "Specify a list of permitted insecure registry server URLs",
		},
	}

	util := []cli.Flag{
		// miscellaneous
		cli.BoolFlag{
			Name:        "use-rp",
			Usage:       "Use resource pool for vch parent in VC instead of a vApp",
			Destination: &c.UseRP,
			Hidden:      true,
		},

		cli.BoolFlag{
			Name:        "force, f",
			Usage:       "Force the install, removing existing if present",
			Destination: &c.Force,
		},
		cli.DurationFlag{
			Name:        "timeout",
			Value:       3 * time.Minute,
			Usage:       "Time to wait for create",
			Destination: &c.Timeout,
		},
	}

	help := []cli.Flag{
		// help options
		cli.BoolFlag{
			Name:        "extended-help, x",
			Usage:       "Show all options - this must be specified instead of --help",
			Destination: &c.advancedOptions,
		},
	}

	target := c.TargetFlags()
	compute := c.ComputeFlags()
	iso := c.ImageFlags(true)
	debug := c.DebugFlags()

	// flag arrays are declared, now combined
	var flags []cli.Flag
	for _, f := range [][]cli.Flag{target, compute, create, iso, util, debug, help} {
		flags = append(flags, f...)
	}

	return flags
}

func (c *Create) processParams() error {
	defer trace.End(trace.Begin(""))

	if err := c.HasCredentials(); err != nil {
		return err
	}

	if c.cert != "" && c.key == "" {
		return cli.NewExitError("key and cert should be specified at the same time", 1)
	}
	if c.cert == "" && c.key != "" {
		return cli.NewExitError("key and cert should be specified at the same time", 1)
	}

	if c.externalNetworkName == "" {
		c.externalNetworkName = "VM Network"
	}

	if c.BridgeNetworkName == "" {
		c.BridgeNetworkName = c.DisplayName
	}

	if len(c.DisplayName) > MaxDisplayNameLen {
		return cli.NewExitError(fmt.Sprintf("Display name %s exceeds the permitted 31 characters limit. Please use a shorter -name parameter", c.DisplayName), 1)
	}

	if err := c.processContainerNetworks(); err != nil {
		return err
	}

	if err := c.processBridgeNetwork(); err != nil {
		return err
	}

	if err := c.processNetwork(&c.Data.ClientNetwork, "client", c.clientNetworkName,
		c.clientNetworkIP, c.clientNetworkGateway); err != nil {
		return err
	}

	if err := c.processNetwork(&c.Data.ExternalNetwork, "external", c.externalNetworkName,
		c.externalNetworkIP, c.externalNetworkGateway); err != nil {
		return err
	}

	if err := c.processNetwork(&c.Data.ManagementNetwork, "management", c.managementNetworkName,
		c.managementNetworkIP, c.managementNetworkGateway); err != nil {
		return err
	}

	if err := c.processDNSServers(); err != nil {
		return err
	}

	// must come after client network processing as it checks for static IP on that interface
	if err := c.processCertificates(); err != nil {
		return err
	}

	if err := c.processVolumeStores(); err != nil {
		return errors.Errorf("Error occurred while processing volume stores: %s", err)
	}

	if err := c.processInsecureRegistries(); err != nil {
		return err
	}

	return nil
}

func (c *Create) processCertificates() error {
	// check for insecure case
	if c.noTLS {
		log.Warn("Configuring without TLS - all communications will be insecure")
		return nil
	}

	// if one or more CAs are provided, then so must the key and cert for host certificate
	cas, keypair, err := c.loadCertificates()
	if err != nil {
		log.Error("Create cannot continue: unable to load certificates")
		return err
	}

	if len(cas) != 0 && keypair == nil {
		log.Error("Create cannot continue: specifying a CA requires --key and --cert parameters")
		return errors.New("If supplying a CA, certificate and key for TLS must also be supplied")
	}

	if len(cas) == 0 && keypair == nil {
		// if we get here we didn't load a CA or keys, so we're generating
		cas, keypair, err = c.generateCertificates(!c.noTLSverify)
		if err != nil {
			log.Error("Create cannot continue: unable to generate certificates")
			return err
		}
	}

	if keypair == nil {
		// this should be caught in earlier error returns, but sanity check
		log.Error("Create cannot continue: unable to load or generate TLS certificates and --no-tls was not specified")
		return err
	}

	c.KeyPEM = keypair.KeyPEM
	c.CertPEM = keypair.CertPEM

	// do we have key, cert, and --no-tlsverify
	if c.noTLSverify || len(cas) == 0 {
		log.Warnf("Configuring without TLS verify - client authentication disabled")
		return nil
	}

	c.ClientCAs = cas
	return nil
}

func (c *Create) processBridgeNetwork() error {
	// bridge network params
	var err error

	_, c.Data.BridgeIPRange, err = net.ParseCIDR(c.BridgeIPRange)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("Error parsing bridge network ip range: %s. Range must be in CIDR format, e.g., 172.16.0.0/12", err), 1)
	}
	return nil
}

func (c *Create) processContainerNetworks() error {
	gws, err := parseContainerNetworkGateways([]string(c.containerNetworksGateway))
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	pools, err := parseContainerNetworkIPRanges([]string(c.containerNetworksIPRanges))
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	dns, err := parseContainerNetworkDNS([]string(c.containerNetworksDNS))
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	// parse container networks
	for _, cn := range c.containerNetworks {
		vnet, v, err := splitVnetParam(cn)
		if err != nil {
			return cli.NewExitError(err.Error(), 1)
		}

		vicnet := vnet
		if v != "" {
			vicnet = v
		}

		c.MappedNetworks[vicnet] = vnet
		c.MappedNetworksGateways[vicnet] = gws[vnet]
		c.MappedNetworksIPRanges[vicnet] = pools[vnet]
		c.MappedNetworksDNS[vicnet] = dns[vnet]

		delete(gws, vnet)
		delete(pools, vnet)
		delete(dns, vnet)
	}

	var hasError bool
	fmtMsg := "The following container network %s is set, but CONTAINER-NETWORK cannot be found. Please check the --container-network and %s settings"
	if len(gws) > 0 {
		log.Error(fmt.Sprintf(fmtMsg, "gateway", "--container-network-gateway"))
		for key, value := range gws {
			mask, _ := value.Mask.Size()
			log.Errorf("\t%s:%s/%d, %q should be vSphere network name", key, value.IP, mask, key)
		}
		hasError = true
	}
	if len(pools) > 0 {
		log.Error(fmt.Sprintf(fmtMsg, "ip range", "--container-network-ip-range"))
		for key, value := range pools {
			log.Errorf("\t%s:%s, %q should be vSphere network name", key, value, key)
		}
		hasError = true
	}
	if len(dns) > 0 {
		log.Errorf(fmt.Sprintf(fmtMsg, "dns", "--container-network-dns"))
		for key, value := range dns {
			log.Errorf("\t%s:%s, %q should be vSphere network name", key, value, key)
		}
		hasError = true
	}
	if hasError {
		return cli.NewExitError("Inconsistent container network configuration.", 1)
	}
	return nil
}

// processNetwork parses network args if present
func (c *Create) processNetwork(network *data.NetworkConfig, netName, pgName, staticIP, gateway string) error {
	network.Name = pgName

	var err error

	i := staticIP != ""
	g := gateway != ""
	if !i && !g {
		return nil
	}
	if i != g {
		return fmt.Errorf("%s network IP and gateway must both be specified", netName)
	}

	defer func(net *data.NetworkConfig) {
		if err == nil {
			log.Debugf("%s network: IP %q gateway %q", netName, net.IP, net.Gateway)
		}
	}(network)

	network.Gateway, err = ip.ParseIPandMask(gateway)
	if err != nil {
		return fmt.Errorf("Invalid %s network gateway: %s", netName, err)
	}

	network.IP, err = ip.ParseIPandMask(staticIP)
	if err == nil {
		return nil
	}

	// try treating it as a name, using the mask from the gateway
	ips, err := net.LookupIP(staticIP)
	if err != nil {
		return fmt.Errorf("Invalid %s network address - neither IP nor resolvable hostname", netName)
	}

	for _, ip := range ips {
		if !network.Gateway.Contains(ip) {
			log.Debugf("Skipping %s as value for %s because it's not in the network specified by gateway", ip.String(), staticIP)
			continue
		}

		if ip.String() != staticIP {
			log.Infof("Assigning %s based on %s", ip.String(), staticIP)
		}

		network.IP = net.IPNet{
			IP:   ip,
			Mask: network.Gateway.Mask,
		}

		return nil
	}

	return fmt.Errorf("Invalid %s network address: %s does not resolve to a gateway compatible IP", netName, staticIP)
}

// processDNSServers parses DNS servers used for client, external, mgmt networks
func (c *Create) processDNSServers() error {
	if len(c.dns) == 0 {
		return nil
	}

	for _, d := range c.dns {
		s := net.ParseIP(d)
		if s == nil {
			return errors.New("Invalid DNS server specified")
		}
		c.Data.DNS = append(c.Data.DNS, s)
	}

	if len(c.Data.DNS) > 3 {
		log.Warn("Maximum of 3 DNS servers. Additional servers specified will be ignored.")
	}

	if c.Data.ClientNetwork.Empty() && c.Data.ExternalNetwork.Empty() && c.Data.ManagementNetwork.Empty() {
		log.Warn("Specified DNS servers are ignored if static IP is not set on any networks. VCH will use DNS servers provided by DHCP.")
	}
	log.Debugf("VCH DNS servers: %s", c.Data.DNS)
	return nil
}

func (c *Create) processVolumeStores() error {
	defer trace.End(trace.Begin(""))
	c.VolumeLocations = make(map[string]string)
	for _, arg := range c.volumeStores {
		splitMeta := strings.SplitN(arg, ":", 2)
		if len(splitMeta) != 2 {
			return errors.New("Volume store input must be in format datastore/path:label")
		}
		c.VolumeLocations[splitMeta[1]] = splitMeta[0]
	}

	return nil
}

func (c *Create) processInsecureRegistries() error {
	for _, registry := range c.insecureRegistries {
		url, err := url.Parse(registry)
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("%s is an invalid format for registry url", registry), 1)
		}
		c.InsecureRegistries = append(c.InsecureRegistries, *url)
	}

	return nil
}

func (c *Create) loadCertificates() ([]byte, *certificate.KeyPair, error) {
	defer trace.End(trace.Begin(""))

	c.envFile = fmt.Sprintf("%s.env", c.DisplayName)

	// reads each of the files specified, assuming that they are PEM encoded certs,
	// and constructs a byte array suitable for passing to CertPool.AppendCertsFromPEM
	var certs []byte
	for _, f := range c.clientCAs {
		log.Infof("Loading CA from %s", f)
		b, err := ioutil.ReadFile(f)
		if err != nil {
			err = errors.Errorf("Failed to load authority from file %s: %s", f, err)
			return nil, nil, err
		}

		certs = append(certs, b...)
	}

	var keypair *certificate.KeyPair
	if c.cert != "" && c.key != "" {
		log.Infof("Loading certificate/key pair - private key in %s", c.key)
		keypair = certificate.NewKeyPair(c.cert, c.key, nil, nil)

		if err := keypair.LoadCertificate(); err != nil {
			log.Errorf("Failed to load certificate: %s", err)
			return certs, nil, err
		}
	}

	return certs, keypair, nil
}

func (c *Create) generateCertificates(ca bool) ([]byte, *certificate.KeyPair, error) {
	defer trace.End(trace.Begin(""))

	var certs []byte
	// generate the certs and keys with names conforming the default the docker client expects
	// to avoid overwriting for a different vch, place this in a directory named for the vch
	err := os.MkdirAll(fmt.Sprintf("./%s", c.DisplayName), 0700)
	if err != nil {
		log.Errorf("Unable to make directory to hold certificates")
		return nil, nil, err
	}

	// the locations for the certificates and env file
	c.envFile = fmt.Sprintf("%s/%[1]s.env", c.DisplayName)

	c.key = fmt.Sprintf("./%s/key.pem", c.DisplayName)
	c.cert = fmt.Sprintf("./%s/cert.pem", c.DisplayName)

	skey := fmt.Sprintf("./%s/server-key.pem", c.DisplayName)
	scert := fmt.Sprintf("./%s/server-cert.pem", c.DisplayName)

	cakey := fmt.Sprintf("./%s/ca-key.pem", c.DisplayName)
	c.cacert = fmt.Sprintf("./%s/ca.pem", c.DisplayName)

	if !ca {
		log.Infof("Generating self-signed certificate/key pair - private key in %s", c.key)
		keypair := certificate.NewKeyPair(c.key, c.cert, nil, nil)
		err := keypair.CreateSelfSigned(c.cname, nil, c.keySize)
		if err != nil {
			log.Errorf("Failed to generate self-signed certificate: %s", err)
			return nil, nil, err
		}

		return certs, keypair, nil
	}

	// if we've not got a specific CommonName but do have a static IP then go with that.
	if c.cname == "" && c.clientNetworkIP != "" {
		c.cname = c.clientNetworkIP
		log.Infof("Using client-network-ip as cname for server certificates - use --tls-cname to override: %s", c.cname)
	}

	if c.cname == "" {
		log.Error("Common Name must be provided when generating certificates for client authentication:")
		log.Info("  --tls-cname=<FQDN or static IP> # for the appliance VM")
		log.Info("  --tls-cname=<*.yourdomain.com>  # if DNS has entries in that form for DHCP addresses (less secure)")
		log.Info("  --no-tlsverify                  # disables client authentication (anyone can connect to the VCH)")
		log.Info("  --no-tls                        # disables TLS entirely")
		log.Info("")

		return certs, nil, errors.New("provide Common Name for server certificate")
	}

	// for now re-use the display name as the organisation if unspecified
	if len(c.org) == 0 {
		c.org = []string{c.DisplayName}
	}
	if len(c.org) == 1 && !strings.HasPrefix(c.cname, "*") {
		// Add in the cname if it's not a wildcard
		c.org = append(c.org, c.cname)
	}

	// Certificate authority
	log.Infof("Generating CA certificate/key pair - private key in %s", cakey)
	cakp := certificate.NewKeyPair(c.cacert, cakey, nil, nil)
	err = cakp.CreateRootCA(c.cname, c.org, c.keySize)
	if err != nil {
		log.Errorf("Failed to generate CA: %s", err)
		return nil, nil, err
	}
	if err = cakp.SaveCertificate(); err != nil {
		log.Errorf("Failed to save CA certificates: %s", err)
		return nil, nil, err
	}

	// Server certificates
	log.Infof("Generating server certificate/key pair - private key in %s", skey)
	skp := certificate.NewKeyPair(scert, skey, nil, nil)
	err = skp.CreateServerCertificate(c.cname, c.org, c.keySize, cakp)
	if err != nil {
		log.Errorf("Failed to generate server certificates: %s", err)
		return nil, nil, err
	}
	if err = skp.SaveCertificate(); err != nil {
		log.Errorf("Failed to save server certificates: %s", err)
		return nil, nil, err
	}

	// Client certificates
	log.Infof("Generating client certificate/key pair - private key in %s", c.key)
	ckp := certificate.NewKeyPair(c.cert, c.key, nil, nil)
	err = ckp.CreateClientCertificate(c.cname, c.org, c.keySize, cakp)
	if err != nil {
		log.Errorf("Failed to generate server certificates: %s", err)
		return nil, nil, err
	}
	if err = ckp.SaveCertificate(); err != nil {
		log.Errorf("Failed to save client certificates: %s", err)
		return nil, nil, err
	}

	c.clientCert, err = ckp.Certificate()
	if err != nil {
		log.Warnf("Failed to stash client certificate for later application level validation: %s", err)
	}

	// If openssl is present, try to generate a browser friendly pfx file (a bundle of the public certificate AND the private key)
	// The pfx file can be imported directly into keychains for client certificate authentication
	args := strings.Split(fmt.Sprintf("pkcs12 -export -out ./%[1]s/cert.pfx -inkey ./%[1]s/key.pem -in ./%[1]s/cert.pem -certfile ./%[1]s/ca.pem -password pass:", c.DisplayName), " ")
	pfx := exec.Command("openssl", args...)
	out, err := pfx.CombinedOutput()
	if err != nil {
		log.Debug(out)
		log.Warnf("Failed to generate browser friendly PFX client certificate: %s", err)
	} else {
		log.Infof("Generated browser friendly PFX client certificate - certificate in ./%s/cert.pfx", c.DisplayName)
	}

	return cakp.CertPEM, skp, nil
}

func (c *Create) Run(cliContext *cli.Context) (err error) {

	if c.advancedOptions {
		cli.HelpPrinter(cliContext.App.Writer, EntireOptionHelpTemplate, cliContext.Command)
		return nil
	}

	if c.Debug.Debug > 0 {
		log.SetLevel(log.DebugLevel)
		trace.Logger.Level = log.DebugLevel
	}
	if err = c.processParams(); err != nil {
		return err
	}

	var images map[string]string
	if images, err = c.CheckImagesFiles(c.Force); err != nil {
		return err
	}

	if len(cliContext.Args()) > 0 {
		log.Errorf("Unknown argument: %s", cliContext.Args()[0])
		return errors.New("invalid CLI arguments")
	}

	log.Infof("### Installing VCH ####")

	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil && ctx.Err() == context.DeadlineExceeded {
			//context deadline exceeded, replace returned error message
			err = errors.Errorf("Create timed out: use --timeout to add more time")
		}
	}()

	validator, err := validate.NewValidator(ctx, c.Data)
	if err != nil {
		log.Error("Create cannot continue: failed to create validator")
		return err
	}

	vchConfig, err := validator.Validate(ctx, c.Data)
	if err != nil {
		log.Error("Create cannot continue: configuration validation failed")
		return err
	}

	vConfig := validator.AddDeprecatedFields(ctx, vchConfig, c.Data)
	vConfig.ImageFiles = images
	vConfig.ApplianceISO = path.Base(c.ApplianceISO)
	vConfig.BootstrapISO = path.Base(c.BootstrapISO)

	vchConfig.InsecureRegistries = c.Data.InsecureRegistries

	if validator.Session.IsVC() { // create certificates for VCH extension
		var certbuffer, keybuffer bytes.Buffer
		if certbuffer, keybuffer, err = certificate.CreateSelfSigned("", []string{"VMware Inc."}, 2048); err != nil {
			return errors.Errorf("Failed to create certificate for VIC vSphere extension: %s", err)
		}
		vchConfig.ExtensionCert = certbuffer.String()
		vchConfig.ExtensionKey = keybuffer.String()
	}

	// separate initial validation from dispatch of creation task
	log.Info("")

	executor := management.NewDispatcher(ctx, validator.Session, vchConfig, c.Force)
	if err = executor.CreateVCH(vchConfig, vConfig); err != nil {

		executor.CollectDiagnosticLogs()
		return err
	}

	// check the docker endpoint is responsive
	if err = executor.CheckDockerAPI(vchConfig, c.clientCert); err != nil {

		executor.CollectDiagnosticLogs()
		return err
	}

	log.Infof("Initialization of appliance successful")

	executor.ShowVCH(vchConfig, c.key, c.cert, c.cacert, c.envFile)
	log.Infof("Installer completed successfully")
	return nil
}

type ipNetUnmarshaler struct {
	ipnet *net.IPNet
	ip    net.IP
}

func (m *ipNetUnmarshaler) UnmarshalText(text []byte) error {
	s := string(text)
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return err
	}

	m.ipnet = ipnet
	m.ip = ip
	return nil
}

func parseContainerNetworkGateways(cgs []string) (map[string]net.IPNet, error) {
	gws := make(map[string]net.IPNet)
	for _, cg := range cgs {
		m := &ipNetUnmarshaler{}
		vnet, err := parseVnetParam(cg, m)
		if err != nil {
			return nil, err
		}

		if _, ok := gws[vnet]; ok {
			return nil, fmt.Errorf("Duplicate gateway specified for container network %s", vnet)
		}

		gws[vnet] = net.IPNet{IP: m.ip, Mask: m.ipnet.Mask}
	}

	return gws, nil
}

func parseContainerNetworkIPRanges(cps []string) (map[string][]ip.Range, error) {
	pools := make(map[string][]ip.Range)
	for _, cp := range cps {
		ipr := &ip.Range{}
		vnet, err := parseVnetParam(cp, ipr)
		if err != nil {
			return nil, err
		}

		pools[vnet] = append(pools[vnet], *ipr)
	}

	return pools, nil
}

func parseContainerNetworkDNS(cds []string) (map[string][]net.IP, error) {
	dns := make(map[string][]net.IP)
	for _, cd := range cds {
		var ip net.IP
		vnet, err := parseVnetParam(cd, &ip)
		if err != nil {
			return nil, err
		}

		if ip == nil {
			return nil, fmt.Errorf("DNS IP not specified for container network %s", vnet)
		}

		dns[vnet] = append(dns[vnet], ip)
	}

	return dns, nil
}

func splitVnetParam(p string) (vnet string, value string, err error) {
	mapped := strings.Split(p, ":")
	if len(mapped) == 0 || len(mapped) > 2 {
		err = fmt.Errorf("Invalid value for parameter %s", p)
		return
	}

	vnet = mapped[0]
	if vnet == "" {
		err = fmt.Errorf("Container network not specified in parameter %s", p)
		return
	}

	if len(mapped) > 1 {
		value = mapped[1]
	}

	return
}

func parseVnetParam(p string, m encoding.TextUnmarshaler) (vnet string, err error) {
	vnet, v, err := splitVnetParam(p)
	if err != nil {
		return "", fmt.Errorf("Error parsing container network parameter %s: %s", p, err)
	}

	if err = m.UnmarshalText([]byte(v)); err != nil {
		return "", fmt.Errorf("Error parsing container network parameter %s: %s", p, err)
	}

	return vnet, nil
}
