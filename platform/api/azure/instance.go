// Copyright 2018 CoreOS, Inc.
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

package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5"

	"github.com/flatcar/mantle/platform/conf"
	"github.com/flatcar/mantle/util"
)

type Machine struct {
	ID               string
	PublicIPAddress  string
	PrivateIPAddress string
	InterfaceName    string
	PublicIPName     string
}

func (a *API) getAvset() string {
	if a.Opts.AvailabilitySet == "" {
		return ""
	}
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/availabilitySets/%s", a.subID, a.Opts.ResourceGroup, a.Opts.AvailabilitySet)
}

func (a *API) getVMRG(rg string) string {
	vmrg := rg
	if a.Opts.ResourceGroup != "" {
		vmrg = a.Opts.ResourceGroup
	}
	return vmrg
}

func (a *API) getVMParameters(name, sshkey, storageAccountURI string, userdata *conf.Conf, ip *armnetwork.PublicIPAddress, nic *armnetwork.Interface) armcompute.VirtualMachine {
	osProfile := armcompute.OSProfile{
		AdminUsername: to.Ptr("core"),
		ComputerName:  &name,
		LinuxConfiguration: &armcompute.LinuxConfiguration{
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{
					{
						Path:    to.Ptr("/home/core/.ssh/authorized_keys"),
						KeyData: &sshkey,
					},
				},
			},
		},
	}

	// Encode userdata to base64.
	ud := base64.StdEncoding.EncodeToString(userdata.Bytes())

	var imgRef *armcompute.ImageReference
	var plan *armcompute.Plan
	if a.Opts.DiskURI != "" {
		imgRef = &armcompute.ImageReference{
			ID: &a.Opts.DiskURI,
		}
	} else {
		imgRef = &armcompute.ImageReference{
			Publisher: &a.Opts.Publisher,
			Offer:     &a.Opts.Offer,
			SKU:       &a.Opts.Sku,
			Version:   &a.Opts.Version,
		}
		if a.Opts.Version == "latest" {
			var top int32 = 1
			vmImgListOpts := &armcompute.VirtualMachineImagesClientListOptions{
				Top:     &top,
				Orderby: to.Ptr("name desc"),
			}
			r, err := a.vmImgClient.List(context.TODO(), a.Opts.Location, a.Opts.Publisher, a.Opts.Offer, a.Opts.Sku, vmImgListOpts)
			if err != nil {
				plog.Warningf("failed to get image list: %v; continuing", err)
			} else if len(r.VirtualMachineImageResourceArray) == 0 || (r.VirtualMachineImageResourceArray[0] == nil) || (r.VirtualMachineImageResourceArray[0].Name == nil) {
				plog.Warningf("no images found; continuing")
			} else {
				a.Opts.Version = *r.VirtualMachineImageResourceArray[0].Name
			}
		}
		// lookup plan information for image
		imgInfo, err := a.vmImgClient.Get(context.TODO(), a.Opts.Location, *imgRef.Publisher, *imgRef.Offer, *imgRef.SKU, *imgRef.Version, nil)
		if err == nil && imgInfo.Properties != nil && imgInfo.Properties.Plan != nil {
			plan = &armcompute.Plan{
				Publisher: imgInfo.Properties.Plan.Publisher,
				Product:   imgInfo.Properties.Plan.Product,
				Name:      imgInfo.Properties.Plan.Name,
			}
			plog.Debugf("using plan: %v:%v:%v", *plan.Publisher, *plan.Product, *plan.Name)
		} else if err != nil {
			plog.Warningf("failed to get image info: %v; continuing", err)
		}
	}
	vm := armcompute.VirtualMachine{
		Name:     &name,
		Location: &a.Opts.Location,
		Tags: map[string]*string{
			"createdBy": to.Ptr("mantle"),
		},
		Plan: plan,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(a.Opts.Size)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: imgRef,
				OSDisk: &armcompute.OSDisk{
					CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					DeleteOption: to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
					},
				},
			},
			OSProfile: &osProfile,
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: nic.ID,
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary:      to.Ptr(true),
							DeleteOption: to.Ptr(armcompute.DeleteOptionsDelete),
						},
					},
				},
			},
			DiagnosticsProfile: &armcompute.DiagnosticsProfile{
				BootDiagnostics: &armcompute.BootDiagnostics{
					Enabled:    to.Ptr(true),
					StorageURI: &storageAccountURI,
				},
			},
		},
	}

	switch a.Opts.DiskController {
	case "nvme":
		vm.Properties.StorageProfile.DiskControllerType = to.Ptr(armcompute.DiskControllerTypesNVMe)
	case "scsi":
		vm.Properties.StorageProfile.DiskControllerType = to.Ptr(armcompute.DiskControllerTypesSCSI)
	}

	// I don't think it would be an issue to have empty user-data set but better
	// to be safe than sorry.
	if ud != "" {
		if a.Opts.UseUserData && userdata.IsIgnition() {
			plog.Infof("using user-data")
			vm.Properties.UserData = &ud
		} else {
			plog.Infof("using custom data")
			vm.Properties.OSProfile.CustomData = &ud
		}
	}

	availabilitySetID := a.getAvset()
	if availabilitySetID != "" {
		vm.Properties.AvailabilitySet = &armcompute.SubResource{ID: &availabilitySetID}
	}

	return vm
}

func (a *API) CreateInstance(name, sshkey, resourceGroup, storageAccount string, userdata *conf.Conf, network Network) (*Machine, error) {
	// only VMs are created in the user supplied resource group, kola still manages a resource group
	// for the gallery and storage account.
	vmResourceGroup := a.getVMRG(resourceGroup)
	subnet := network.subnet

	ip, err := a.createPublicIP(resourceGroup)
	if err != nil {
		return nil, fmt.Errorf("creating public ip: %v", err)
	}
	if ip.Name == nil {
		return nil, fmt.Errorf("couldn't get public IP name")
	}

	nic, err := a.createNIC(ip, &subnet, resourceGroup)
	if err != nil {
		return nil, fmt.Errorf("creating nic: %v", err)
	}
	if nic.Name == nil {
		return nil, fmt.Errorf("couldn't get NIC name")
	}

	vmParams := a.getVMParameters(name, sshkey, fmt.Sprintf("https://%s.blob.core.windows.net/", storageAccount), userdata, ip, nic)
	plog.Infof("Creating Instance %s", name)

	clean := func() {
		_, _ = a.compClient.BeginDelete(context.TODO(), vmResourceGroup, name, &armcompute.VirtualMachinesClientBeginDeleteOptions{
			ForceDeletion: to.Ptr(true),
		})
		_, _ = a.intClient.BeginDelete(context.TODO(), resourceGroup, *nic.Name, nil)
		_, _ = a.ipClient.BeginDelete(context.TODO(), resourceGroup, *ip.Name, nil)
	}

	poller, err := a.compClient.BeginCreateOrUpdate(context.TODO(), vmResourceGroup, name, vmParams, nil)
	if err != nil {
		clean()
		return nil, err
	}
	_, err = poller.PollUntilDone(context.TODO(), nil)
	if err != nil {
		clean()
		return nil, err
	}
	plog.Infof("Instance %s created", name)

	err = util.WaitUntilReady(5*time.Minute, 10*time.Second, func() (bool, error) {
		vm, err := a.compClient.Get(context.TODO(), vmResourceGroup, name, nil)
		if err != nil {
			return false, err
		}

		if vm.Properties != nil && vm.Properties.ProvisioningState != nil && *vm.Properties.ProvisioningState != "Succeeded" {
			return false, nil
		}

		return true, nil
	})
	plog.Infof("Instance %s ready", name)
	if err != nil {
		clean()
		return nil, fmt.Errorf("waiting for machine to become active: %v", err)
	}

	vm, err := a.compClient.Get(context.TODO(), vmResourceGroup, name, nil)
	if err != nil {
		return nil, err
	}

	if vm.Name == nil {
		return nil, fmt.Errorf("couldn't get VM ID")
	}
	ipName := *ip.Name
	if a.Opts.UsePrivateIPs {
		// empty IP name means instance is accessible via private IP address
		ipName = ""
	}
	publicaddr, privaddr, err := a.GetIPAddresses(*nic.Name, ipName, resourceGroup)
	if err != nil {
		return nil, err
	}

	return &Machine{
		ID:               *vm.Name,
		PublicIPAddress:  publicaddr,
		PrivateIPAddress: privaddr,
		InterfaceName:    *nic.Name,
		PublicIPName:     ipName,
	}, nil
}

// TerminateInstance deletes a VM created by CreateInstance. Public IP, NIC and
// OS disk are deleted automatically together with the VM.
func (a *API) TerminateInstance(machine *Machine, resourceGroup string) error {
	resourceGroup = a.getVMRG(resourceGroup)
	_, err := a.compClient.BeginDelete(context.TODO(), resourceGroup, machine.ID, &armcompute.VirtualMachinesClientBeginDeleteOptions{
		ForceDeletion: to.Ptr(true),
	})
	// We used to wait for the VM to be deleted here, but it's not necessary as
	// we will also delete the resource group later.
	return err
}

func (a *API) GetConsoleOutput(name, resourceGroup, storageAccount string) ([]byte, error) {
	vmResourceGroup := a.getVMRG(resourceGroup)
	vm, err := a.compClient.Get(context.TODO(), vmResourceGroup, name, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return nil, fmt.Errorf("could not get VM: %v", err)
	}

	consoleURI := vm.Properties.InstanceView.BootDiagnostics.SerialConsoleLogBlobURI
	if consoleURI == nil {
		return nil, fmt.Errorf("serial console URI is nil")
	}

	// Only the full URI to the logs are present in the virtual machine
	// properties. Parse out the container & file name to use the GetBlob
	// API call directly.
	uri := []byte(*consoleURI)
	containerPat := regexp.MustCompile(`bootdiagnostics-[a-z0-9\-]+`)
	container := string(containerPat.Find(uri))
	if container == "" {
		return nil, fmt.Errorf("could not find container name in URI: %q", *consoleURI)
	}
	namePat := regexp.MustCompile(`[a-z0-9\-\.]+.serialconsole.log`)
	blobname := string(namePat.Find(uri))
	if blobname == "" {
		return nil, fmt.Errorf("could not find blob name in URI: %q", *consoleURI)
	}

	client, err := a.GetBlobServiceClient(storageAccount)
	if err != nil {
		return nil, err
	}
	var data io.ReadCloser
	err = util.Retry(6, 10*time.Second, func() error {
		data, err = GetBlob(client, container, blobname)
		if err != nil {
			return fmt.Errorf("could not get blob for container %q, blobname %q: %v", container, blobname, err)
		}
		if data == nil {
			return fmt.Errorf("empty data while getting blob for container %q, blobname %q", container, blobname)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return io.ReadAll(data)
}
