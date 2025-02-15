// Copyright 2017 CoreOS, Inc.
// Copyright 2018 Red Hat
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package equinixmetal

import (
	"github.com/coreos/pkg/capnslog"

	ctplatform "github.com/flatcar/container-linux-config-transpiler/config/platform"
	"github.com/flatcar/mantle/platform"
	"github.com/flatcar/mantle/platform/api/equinixmetal"
)

const (
	Platform platform.Name = "equinixmetal"
)

var (
	plog = capnslog.NewPackageLogger("github.com/flatcar/mantle", "platform/machine/equinixmetal")
)

type flight struct {
	*platform.BaseFlight
	api      *equinixmetal.API
	sshKeyID string
	// devicesPool holds the devices available
	// to be recycled by EM in order to minimize the
	// number of created devices.
	devicesPool chan string
}

func NewFlight(opts *equinixmetal.Options) (platform.Flight, error) {
	api, err := equinixmetal.New(opts)
	if err != nil {
		return nil, err
	}

	bf, err := platform.NewBaseFlight(opts.Options, Platform, ctplatform.Packet)
	if err != nil {
		return nil, err
	}

	pf := &flight{
		BaseFlight:  bf,
		api:         api,
		devicesPool: make(chan string, 1000),
	}

	keys, err := pf.Keys()
	if err != nil {
		pf.Destroy()
		return nil, err
	}
	pf.sshKeyID, err = pf.api.AddKey(pf.Name(), keys[0].String())
	if err != nil {
		pf.Destroy()
		return nil, err
	}

	return pf, nil
}

func (pf *flight) NewCluster(rconf *platform.RuntimeConfig) (platform.Cluster, error) {
	bc, err := platform.NewBaseCluster(pf.BaseFlight, rconf)
	if err != nil {
		return nil, err
	}

	pc := &cluster{
		BaseCluster: bc,
		flight:      pf,
	}
	if !rconf.NoSSHKeyInMetadata {
		pc.sshKeyID = pf.sshKeyID
	}

	pf.AddCluster(pc)

	return pc, nil
}

func (pf *flight) Destroy() {
	if pf.api != nil {
		if err := pf.api.Close(); err != nil {
			plog.Errorf("closing API %v: ", err)
		}
	}

	if pf.sshKeyID != "" {
		if err := pf.api.DeleteKey(pf.sshKeyID); err != nil {
			plog.Errorf("Error deleting key %v: %v", pf.sshKeyID, err)
		}
	}

	// before delete the instances from the devices pool
	// we close it in order to avoid deadlocks.
	close(pf.devicesPool)
	for id := range pf.devicesPool {
		if err := pf.api.DeleteDevice(id); err != nil {
			plog.Errorf("deleting device %s: %v", id, err)
		}
	}

	pf.BaseFlight.Destroy()
}
