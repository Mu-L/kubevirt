/*
This file is part of the KubeVirt project

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Copyright The KubeVirt Authors.
*/

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
	v1alpha1 "kubevirt.io/client-go/kubevirt/typed/instancetype/v1alpha1"
)

type FakeInstancetypeV1alpha1 struct {
	*testing.Fake
}

func (c *FakeInstancetypeV1alpha1) VirtualMachineClusterInstancetypes() v1alpha1.VirtualMachineClusterInstancetypeInterface {
	return &FakeVirtualMachineClusterInstancetypes{c}
}

func (c *FakeInstancetypeV1alpha1) VirtualMachineClusterPreferences() v1alpha1.VirtualMachineClusterPreferenceInterface {
	return &FakeVirtualMachineClusterPreferences{c}
}

func (c *FakeInstancetypeV1alpha1) VirtualMachineInstancetypes(namespace string) v1alpha1.VirtualMachineInstancetypeInterface {
	return &FakeVirtualMachineInstancetypes{c, namespace}
}

func (c *FakeInstancetypeV1alpha1) VirtualMachinePreferences(namespace string) v1alpha1.VirtualMachinePreferenceInterface {
	return &FakeVirtualMachinePreferences{c, namespace}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeInstancetypeV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
