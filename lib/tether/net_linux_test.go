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

// +build linux

package tether

import (
	"io/ioutil"
	"net"
	"strconv"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/stretchr/testify/assert"

	"github.com/vmware/vic/lib/config/executor"
	"github.com/vmware/vic/lib/etcconf"
)

// Utility method to add an interface to Mocked
// This assigns the interface name and returns the "slot" as a string
func AddInterface(name string, mocker *Mocker) string {
	mocker.maxSlot++

	mocker.Interfaces[name] = &Interface{
		LinkAttrs: netlink.LinkAttrs{
			Name:  name,
			Index: mocker.maxSlot,
		},
		Up: true,
	}

	return strconv.Itoa(mocker.maxSlot)
}

func TestSetIpAddress(t *testing.T) {
	_, mocker := testSetup(t)
	defer testTeardown(t, mocker)

	hFile, err := ioutil.TempFile("", "vic_set_ip_test_hosts")
	if err != nil {
		t.Errorf("Failed to create tmp hosts file: %s", err)
	}
	rFile, err := ioutil.TempFile("", "vic_set_ip_test_resolv")
	if err != nil {
		t.Errorf("Failed to create tmp resolv file: %s", err)
	}

	// give us a hosts file we can modify
	defer func(hosts etcconf.Hosts, resolv etcconf.ResolvConf) {
		Sys.Hosts = hosts
		Sys.ResolvConf = resolv
	}(Sys.Hosts, Sys.ResolvConf)

	Sys.Hosts = etcconf.NewHosts(hFile.Name())
	Sys.ResolvConf = etcconf.NewResolvConf(rFile.Name())

	bridge := AddInterface("eth1", mocker)
	external := AddInterface("eth2", mocker)

	secondIP, _ := netlink.ParseIPNet("172.16.0.10/24")
	gwIP, _ := netlink.ParseIPNet("172.16.0.1/24")
	cfg := executor.ExecutorConfig{
		Common: executor.Common{
			ID:   "ipconfig",
			Name: "tether_test_executor",
		},
		Networks: map[string]*executor.NetworkEndpoint{
			"bridge": {
				Common: executor.Common{
					ID: bridge,
					// interface rename
					Name: "bridge",
				},
				Network: executor.ContainerNetwork{
					Common: executor.Common{
						Name: "bridge",
					},
					Default: true,
					Gateway: *gwIP,
				},
				Static: true,
				IP: &net.IPNet{
					IP:   localhost,
					Mask: lmask.Mask,
				},
			},
			"cnet": {
				Common: executor.Common{
					ID: bridge,
					// no interface rename
				},
				Network: executor.ContainerNetwork{
					Common: executor.Common{
						Name: "cnet",
					},
				},
				Static: true,
				IP:     secondIP,
			},
			"external": {
				Common: executor.Common{
					ID: external,
					// interface rename
					Name: "external",
				},
				Network: executor.ContainerNetwork{
					Common: executor.Common{
						Name: "external",
					},
				},
				Static: true,
				IP: &net.IPNet{
					IP:   gateway,
					Mask: gmask.Mask,
				},
			},
		},
	}

	tthr, _ := StartTether(t, &cfg, mocker)

	defer func() {
		// prevent indefinite wait in tether - normally session exit would trigger this
		tthr.Stop()

		// wait for tether to exit
		<-mocker.Cleaned
	}()

	<-mocker.Started

	assert.NotNil(t, mocker.Interfaces["bridge"], "Expected bridge network if endpoints applied correctly")
	// check addresses
	bIface, _ := mocker.Interfaces["bridge"].(*Interface)
	assert.NotNil(t, bIface)

	assert.Equal(t, 2, len(bIface.Addrs), "Expected two addresses on bridge interface")

	eIface, _ := mocker.Interfaces["external"].(*Interface)
	assert.NotNil(t, eIface)

	assert.Equal(t, 1, len(eIface.Addrs), "Expected one address on external interface")
}
