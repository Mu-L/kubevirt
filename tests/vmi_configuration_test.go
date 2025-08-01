/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package tests_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	expect "github.com/google/goexpect"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	k8sv1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/libdv"
	"kubevirt.io/kubevirt/pkg/libvmi"
	libvmici "kubevirt.io/kubevirt/pkg/libvmi/cloudinit"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
	hw_utils "kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"

	"kubevirt.io/kubevirt/tests/console"
	cd "kubevirt.io/kubevirt/tests/containerdisk"
	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/exec"
	"kubevirt.io/kubevirt/tests/framework/checks"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/framework/matcher"
	. "kubevirt.io/kubevirt/tests/framework/matcher"
	"kubevirt.io/kubevirt/tests/libdomain"
	"kubevirt.io/kubevirt/tests/libkubevirt"
	kvconfig "kubevirt.io/kubevirt/tests/libkubevirt/config"
	"kubevirt.io/kubevirt/tests/libnet"
	"kubevirt.io/kubevirt/tests/libnet/cloudinit"
	"kubevirt.io/kubevirt/tests/libnode"
	"kubevirt.io/kubevirt/tests/libpod"
	"kubevirt.io/kubevirt/tests/libsecret"
	"kubevirt.io/kubevirt/tests/libstorage"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/libvmops"
	"kubevirt.io/kubevirt/tests/libwait"
	"kubevirt.io/kubevirt/tests/storage"
	"kubevirt.io/kubevirt/tests/testsuite"
	"kubevirt.io/kubevirt/tests/watcher"
)

var _ = Describe("[sig-compute]Configurations", decorators.SigCompute, func() {
	const enoughMemForSafeBiosEmulation = "32Mi"
	var virtClient kubecli.KubevirtClient

	const (
		cgroupV1MemoryUsagePath = "/sys/fs/cgroup/memory/memory.usage_in_bytes"
		cgroupV2MemoryUsagePath = "/sys/fs/cgroup/memory.current"
	)

	getPodMemoryUsage := func(pod *k8sv1.Pod) (output string, err error) {
		output, err = exec.ExecuteCommandOnPod(
			pod,
			"compute",
			[]string{"cat", cgroupV2MemoryUsagePath},
		)

		if err == nil {
			return
		}

		output, err = exec.ExecuteCommandOnPod(
			pod,
			"compute",
			[]string{"cat", cgroupV1MemoryUsagePath},
		)

		return
	}

	BeforeEach(func() {
		virtClient = kubevirt.Client()
	})

	Context("when requesting virtio-transitional models", func() {
		It("[test_id:6957]should start and run the guest", func() {
			vmi := libvmifact.NewCirros(
				libvmi.WithRng(),
				libvmi.WithWatchdog(v1.WatchdogActionPoweroff, libnode.GetArch()),
				libvmi.WithTablet("tablet", "virtio"),
				libvmi.WithTablet("tablet1", "usb"),
			)
			vmi.Spec.Domain.Devices.UseVirtioTransitional = pointer.P(true)
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			Expect(console.LoginToCirros(vmi)).To(Succeed())
			domSpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())
			testutils.ExpectVirtioTransitionalOnly(domSpec)
		})
	})

	Context("[rfe_id:897][crit:medium][vendor:cnv-qe@redhat.com][level:component]for CPU and memory limits should", func() {

		It("[test_id:3110]lead to get the burstable QOS class assigned when limit and requests differ", decorators.Conformance, func() {
			vmi := libvmops.RunVMIAndExpectScheduling(libvmifact.NewAlpine(), 60)

			Eventually(func() k8sv1.PodQOSClass {
				vmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				if vmi.Status.QOSClass == nil {
					return ""
				}
				return *vmi.Status.QOSClass
			}, 10*time.Second, 1*time.Second).Should(Equal(k8sv1.PodQOSBurstable))
		})

		It("[test_id:3111]lead to get the guaranteed QOS class assigned when limit and requests are identical", decorators.Conformance, func() {
			vmi := libvmifact.NewAlpine(
				libvmi.WithCPURequest("1"), libvmi.WithMemoryRequest("64M"),
				libvmi.WithCPULimit("1"), libvmi.WithMemoryLimit("64M"),
			)
			vmi = libvmops.RunVMIAndExpectScheduling(vmi, 60)

			Eventually(func() k8sv1.PodQOSClass {
				vmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				if vmi.Status.QOSClass == nil {
					return ""
				}
				return *vmi.Status.QOSClass
			}, 10*time.Second, 1*time.Second).Should(Equal(k8sv1.PodQOSGuaranteed))
		})

		It("[test_id:3112]lead to get the guaranteed QOS class assigned when only limits are set", decorators.Conformance, func() {
			vmi := libvmifact.NewAlpine(
				libvmi.WithCPULimit("1"), libvmi.WithMemoryLimit("128Mi"),
			)
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{}

			vmi = libvmops.RunVMIAndExpectScheduling(vmi, 60)

			Eventually(func() k8sv1.PodQOSClass {
				vmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				if vmi.Status.QOSClass == nil {
					return ""
				}
				return *vmi.Status.QOSClass
			}, 10*time.Second, 1*time.Second).Should(Equal(k8sv1.PodQOSGuaranteed))

			vmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(vmi.Spec.Domain.Resources.Requests.Cpu().Cmp(*vmi.Spec.Domain.Resources.Limits.Cpu())).To(BeZero(), "Requests and Limits for CPU on VMI should match")
			Expect(vmi.Spec.Domain.Resources.Requests.Memory().Cmp(*vmi.Spec.Domain.Resources.Limits.Memory())).To(BeZero(), "Requests and Limits for memory on VMI should match")
		})

	})

	Describe("VirtualMachineInstance definition", func() {
		fedoraWithUefiSecuredBoot := libvmifact.NewFedora(
			libvmi.WithMemoryRequest("1Gi"),
			libvmi.WithUefi(true),
			libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
			libvmi.WithNetwork(v1.DefaultPodNetwork()),
		)
		alpineWithUefiWithoutSecureBoot := libvmifact.NewAlpine(
			libvmi.WithMemoryRequest("1Gi"),
			libvmi.WithUefi(false),
			libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
			libvmi.WithNetwork(v1.DefaultPodNetwork()),
		)

		DescribeTable("with memory configuration", func(vmiOptions []libvmi.Option, expectedGuestMemory int) {
			vmi := libvmi.New(vmiOptions...)

			By("Starting a VirtualMachineInstance")
			vmi = libvmops.RunVMIAndExpectScheduling(vmi, 60)
			libwait.WaitForSuccessfulVMIStart(vmi)

			expectedMemoryInKiB := expectedGuestMemory * 1024
			expectedMemoryXMLStr := fmt.Sprintf("unit='KiB'>%d", expectedMemoryInKiB)

			domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
			Expect(err).ToNot(HaveOccurred())
			Expect(domXml).To(ContainSubstring(expectedMemoryXMLStr))

		},
			Entry("provided by domain spec directly",
				[]libvmi.Option{
					libvmi.WithGuestMemory("512Mi"),
				},
				512,
			),
			Entry("provided by resources limits",
				[]libvmi.Option{
					libvmi.WithMemoryLimit("256Mi"),
					libvmi.WithCPULimit("1"),
				},
				256,
			),
			Entry("provided by resources requests and limits",
				[]libvmi.Option{
					libvmi.WithCPURequest("1"),
					libvmi.WithCPULimit("1"),
					libvmi.WithMemoryRequest("64Mi"),
					libvmi.WithMemoryLimit("256Mi"),
				},
				64,
			),
		)

		Context("[rfe_id:2065][crit:medium][vendor:cnv-qe@redhat.com][level:component]with 3 CPU cores", Serial, func() {
			var availableNumberOfCPUs int

			BeforeEach(func() {
				availableNumberOfCPUs = libnode.GetHighestCPUNumberAmongNodes(virtClient)

				requiredNumberOfCpus := 3
				Expect(availableNumberOfCPUs).ToNot(BeNumerically("<", requiredNumberOfCpus),
					fmt.Sprintf("Test requires %d cpus, but only %d available!", requiredNumberOfCpus, availableNumberOfCPUs))
			})

			It("[test_id:1659]should report 3 cpu cores under guest OS", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithCPUCount(3, 0, 0),
					libvmi.WithMemoryRequest("128Mi"),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of CPU cores under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^processor /proc/cpuinfo\n"},
					&expect.BExp{R: console.RetValue("3")},
				}, 15)).To(Succeed(), "should report number of cores")

				By("Checking the requested amount of memory allocated for a guest")
				Expect(vmi.Spec.Domain.Resources.Requests.Memory().String()).To(Equal("128Mi"))

				readyPod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())

				var computeContainer *k8sv1.Container
				for _, container := range readyPod.Spec.Containers {
					if container.Name == "compute" {
						computeContainer = &container
						break
					}
				}
				Expect(computeContainer).ToNot(BeNil(), "could not find the compute container")
				Expect(computeContainer.Resources.Requests.Memory().ToDec().ScaledValue(resource.Mega)).To(Equal(int64(399)))
			})
			It("[test_id:4624]should set a correct memory units", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithMemoryRequest("128Mi"),
				)
				expectedMemoryInKiB := 128 * 1024
				expectedMemoryXMLStr := fmt.Sprintf("unit='KiB'>%d", expectedMemoryInKiB)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
				Expect(err).ToNot(HaveOccurred())
				Expect(domXml).To(ContainSubstring(expectedMemoryXMLStr))
			})

			It("[test_id:1660]should report 3 sockets under guest OS", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithCPUCount(2, 0, 3),
					libvmi.WithMemoryRequest("128Mi"),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of sockets under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep '^physical id' /proc/cpuinfo | uniq | wc -l\n"},
					&expect.BExp{R: console.RetValue("3")},
				}, 60)).To(Succeed(), "should report number of sockets")
			})

			It("[test_id:1661]should report 2 sockets from spec.domain.resources.requests under guest OS ", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithCPURequest("1200m"),
					libvmi.WithMemoryRequest("128Mi"),
				)
				vmi.Spec.Domain.CPU = nil

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of sockets under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep '^physical id' /proc/cpuinfo | uniq | wc -l\n"},
					&expect.BExp{R: console.RetValue("2")},
				}, 60)).To(Succeed(), "should report number of sockets")
			})

			It("[test_id:1662]should report 2 sockets from spec.domain.resources.limits under guest OS ", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithCPULimit("1200m"),
					libvmi.WithMemoryRequest("128Mi"),
				)
				vmi.Spec.Domain.CPU = nil

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of sockets under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep '^physical id' /proc/cpuinfo | uniq | wc -l\n"},
					&expect.BExp{R: console.RetValue("2")},
				}, 60)).To(Succeed(), "should report number of sockets")
			})

			It("[test_id:1663]should report 4 vCPUs under guest OS", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithCPUCount(1, 2, 2),
					libvmi.WithMemoryRequest("128M"),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of vCPUs under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^processor /proc/cpuinfo\n"},
					&expect.BExp{R: console.RetValue("4")},
				}, 60)).To(Succeed(), "should report number of threads")
			})

			It("[test_id:1664]should map cores to virtio block queues", Serial, func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithMemoryRequest("128Mi"),
					libvmi.WithCPURequest("3"),
				)
				vmi.Spec.Domain.Devices.BlockMultiQueue = pointer.P(true)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
				Expect(err).ToNot(HaveOccurred())
				Expect(domXml).To(ContainSubstring("queues='3'"))
			})

			It("[test_id:1665]should map cores to virtio net queues", func() {
				vmi := libvmifact.NewAlpine()
				_true := true
				_false := false
				vmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("128Mi"),
						k8sv1.ResourceCPU:    resource.MustParse("3"),
					},
				}

				vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue = &_true
				vmi.Spec.Domain.Devices.BlockMultiQueue = &_false

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
				Expect(err).ToNot(HaveOccurred())
				Expect(domXml).To(ContainSubstring("driver name='vhost' queues='3'"))
				// make sure that there are not block queues configured
				Expect(domXml).ToNot(ContainSubstring("cache='none' queues='3'"))
			})

			It("[test_id:1667]should not enforce explicitly rejected virtio block queues without cores", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithMemoryRequest("128Mi"),
				)
				vmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}
				vmi.Spec.Domain.Devices.BlockMultiQueue = pointer.P(false)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
				Expect(err).ToNot(HaveOccurred())
				Expect(domXml).ToNot(ContainSubstring("queues='"))
			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with no memory requested", func() {
			It("[test_id:3113]should failed to the VMI creation", func() {
				vmi := libvmi.New()
				By("Starting a VirtualMachineInstance")
				_, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
			})
		})

		Context("[rfe_id:609][crit:medium][vendor:cnv-qe@redhat.com][level:component]with cluster memory overcommit being applied", Serial, func() {
			BeforeEach(func() {
				kv := libkubevirt.GetCurrentKv(virtClient)

				config := kv.Spec.Configuration
				config.DeveloperConfiguration.MemoryOvercommit = 200
				kvconfig.UpdateKubeVirtConfigValueAndWait(config)
			})

			It("[test_id:3114]should set requested amount of memory according to the specified virtual memory", func() {
				vmi := libvmi.New()
				guestMemory := resource.MustParse("4096M")
				vmi.Spec.Domain.Memory = &v1.Memory{Guest: &guestMemory}
				vmi.Spec.Domain.Resources = v1.ResourceRequirements{}
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Spec.Domain.Resources.Requests.Memory().String()).To(Equal("2048M"))
			})
		})

		Context("with BIOS bootloader method and no disk", func() {
			It("[test_id:5265]should find no bootable device by default", func() {
				By("Creating a VMI with no disk and an explicit network interface")
				vmi := libvmi.New(
					libvmi.WithNetwork(v1.DefaultPodNetwork()),
					libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				)
				vmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("32M"),
					},
				}

				By("Enabling BIOS serial output")
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					Bootloader: &v1.Bootloader{
						BIOS: &v1.BIOS{
							UseSerial: pointer.P(true),
						},
					},
				}

				By("Ensuring network boot is disabled on the network interface")
				Expect(vmi.Spec.Domain.Devices.Interfaces[0].BootOrder).To(BeNil())

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Expecting no bootable NIC")
				Expect(console.NetBootExpecter(vmi)).NotTo(Succeed())
				// The expecter *should* have error-ed since the network interface is not marked bootable
			})

			It("[test_id:5266]should boot to NIC rom if a boot order was set on a network interface", func() {
				By("Enabling network boot")
				var bootOrder uint = 1
				interfaceDeviceWithMasqueradeBinding := libvmi.InterfaceDeviceWithMasqueradeBinding()
				interfaceDeviceWithMasqueradeBinding.BootOrder = &bootOrder

				By("Creating a VMI with no disk and an explicit network interface")
				vmi := libvmi.New(
					libvmi.WithMemoryRequest(enoughMemForSafeBiosEmulation),
					libvmi.WithNetwork(v1.DefaultPodNetwork()),
					libvmi.WithInterface(interfaceDeviceWithMasqueradeBinding),
					withSerialBIOS(),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Expecting a bootable NIC")
				Expect(console.NetBootExpecter(vmi)).To(Succeed())
			})
		})

		Context("with ACPI table", func() {
			It("Should configure guest ACPI SLIC with Secret file", func() {
				const (
					volumeSlicSecretName = "volume-slic-secret"
					secretWithSlicName   = "secret-with-slic-data"
				)
				var slicTable = []byte{
					0x53, 0x4c, 0x49, 0x43, 0x24, 0x00, 0x00, 0x00, 0x01, 0x49, 0x43, 0x52,
					0x41, 0x53, 0x48, 0x20, 0x4d, 0x45, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
					0x88, 0x04, 0x00, 0x00, 0x71, 0x65, 0x6d, 0x75, 0x00, 0x00, 0x00, 0x00,
				}
				vmi := libvmifact.NewAlpine()

				By("Creating a secret with the binary ACPI SLIC table")
				secret := libsecret.New(secretWithSlicName, libsecret.DataBytes{"slic.bin": slicTable})
				_, err := virtClient.CoreV1().Secrets(testsuite.GetTestNamespace(vmi)).Create(context.Background(), secret, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Configuring the volume with the secret")
				vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
					Name: volumeSlicSecretName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: secretWithSlicName,
						},
					},
				})

				// The firmware needs to reference the volume name of slic secret
				By("Configuring the firmware option with volume name that contains the secret")
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					ACPI: &v1.ACPI{
						SlicNameRef: volumeSlicSecretName,
					},
				}
				vmi = libvmops.RunVMIAndExpectLaunch(vmi, 360)
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the guest ACPI SLIC table matches the one provided")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "xxd -p -c 40 /sys/firmware/acpi/tables/SLIC\n"},
					&expect.BExp{R: console.RetValue(hex.EncodeToString(slicTable))},
				}, 3)).To(Succeed())
			})

			It("Should configure guest ACPI MSDM with Secret file", func() {
				const (
					volumeMsdmSecretName = "volume-msdm-secret"
					secretWithMsdmName   = "secret-with-msdm-data"
				)
				var msdmTable = []byte{
					0x4d, 0x53, 0x44, 0x4d, 0x24, 0x00, 0x00, 0x00, 0x01, 0x43, 0x43, 0x52,
					0x41, 0x53, 0x48, 0x20, 0x4d, 0x45, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
					0x88, 0x04, 0x00, 0x00, 0x71, 0x65, 0x6d, 0x75, 0x00, 0x00, 0x00, 0x00,
				}
				vmi := libvmifact.NewAlpine()

				By("Creating a secret with the binary ACPI msdm table")
				secret := libsecret.New(secretWithMsdmName, libsecret.DataBytes{"msdm.bin": msdmTable})
				_, err := virtClient.CoreV1().Secrets(testsuite.GetTestNamespace(vmi)).Create(context.Background(), secret, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Configuring the volume with the secret")
				vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
					Name: volumeMsdmSecretName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: secretWithMsdmName,
						},
					},
				})

				// The firmware needs to reference the volume name of msdm secret
				By("Configuring the firmware option with volume name that contains the secret")
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					ACPI: &v1.ACPI{
						MsdmNameRef: volumeMsdmSecretName,
					},
				}
				vmi = libvmops.RunVMIAndExpectLaunch(vmi, 360)
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the guest ACPI MSDM table matches the one provided")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "xxd -p -c 40 /sys/firmware/acpi/tables/MSDM\n"},
					&expect.BExp{R: console.RetValue(hex.EncodeToString(msdmTable))},
				}, 3)).To(Succeed())
			})
		})

		DescribeTable("[rfe_id:2262][crit:medium][vendor:cnv-qe@redhat.com][level:component]with EFI bootloader method", func(vmi *v1.VirtualMachineInstance, loginTo console.LoginToFunction, msg string, fileName string) {
			By("Starting a VirtualMachineInstance")
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			wp := watcher.WarningsPolicy{FailOnWarnings: false}
			libwait.WaitForVMIPhase(vmi,
				[]v1.VirtualMachineInstancePhase{v1.Running, v1.Failed},
				libwait.WithWarningsPolicy(&wp),
				libwait.WithTimeout(180),
				libwait.WithWaitForFail(true),
			)
			vmiMeta, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
			ExpectWithOffset(1, err).ToNot(HaveOccurred())

			switch vmiMeta.Status.Phase {
			case v1.Failed:
				// This Error is expected to be handled
				By("Getting virt-launcher logs")
				logs := func() string { return getVirtLauncherLogs(virtClient, vmi) }
				Eventually(logs,
					30*time.Second,
					500*time.Millisecond).
					Should(ContainSubstring("EFI OVMF rom missing"))
			default:
				libwait.WaitUntilVMIReady(vmi, loginTo)
				By(msg)
				domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
				Expect(err).ToNot(HaveOccurred())
				Expect(domXml).To(MatchRegexp(fileName))
			}
		},
			Entry("[test_id:1668]should use EFI without secure boot", Serial, alpineWithUefiWithoutSecureBoot, console.LoginToAlpine, "Checking if UEFI is enabled", `OVMF_CODE(\.secboot)?\.fd`),
			Entry("[test_id:4437]should enable EFI secure boot", Serial, fedoraWithUefiSecuredBoot, console.SecureBootExpecter, "Checking if SecureBoot is enabled in the libvirt XML", `OVMF_CODE\.secboot\.fd`),
		)

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with diverging guest memory from requested memory", func() {
			It("[test_id:1669]should show the requested guest memory inside the VMI", func() {
				vmi := libvmifact.NewCirros()
				guestMemory := resource.MustParse("256Mi")
				vmi.Spec.Domain.Memory = &v1.Memory{
					Guest: &guestMemory,
				}

				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				Expect(console.LoginToCirros(vmi)).To(Succeed())

				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "free -m | grep Mem: | tr -s ' ' | cut -d' ' -f2\n"},
					&expect.BExp{R: console.RetValue("225")},
				}, 10)).To(Succeed())

			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with diverging memory limit from memory request and no guest memory", func() {
			It("[test_id:3115]should show the memory request inside the VMI", func() {
				vmi := libvmifact.NewCirros(
					libvmi.WithMemoryRequest("256Mi"),
					libvmi.WithMemoryLimit("512Mi"),
				)
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				Expect(console.LoginToCirros(vmi)).To(Succeed())

				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "free -m | grep Mem: | tr -s ' ' | cut -d' ' -f2\n"},
					&expect.BExp{R: console.RetValue("225")},
				}, 10)).To(Succeed())

			})
		})

		Context("[rfe_id:989]test cpu_allocation_ratio", func() {
			It("virt-launchers pod cpu requests should be proportional to the number of vCPUs", func() {
				vmi := libvmifact.NewCirros()
				guestMemory := resource.MustParse("256Mi")
				vmi.Spec.Domain.Memory = &v1.Memory{
					Guest: &guestMemory,
				}
				vmi.Spec.Domain.CPU = &v1.CPU{
					Threads: 1,
					Sockets: 1,
					Cores:   6,
				}

				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				readyPod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())

				var computeContainer *k8sv1.Container
				for _, container := range readyPod.Spec.Containers {
					if container.Name == "compute" {
						computeContainer = &container
						break
					}
				}
				Expect(computeContainer).ToNot(BeNil(), "could not find the computer container")
				Expect(computeContainer.Resources.Requests.Cpu().String()).To(Equal("600m"))
			})

		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with support memory over commitment", func() {
			It("[test_id:755]should show the requested memory different than guest memory", func() {
				vmi := libvmifact.NewCirros(overcommitGuestOverhead())
				guestMemory := resource.MustParse("256Mi")
				vmi.Spec.Domain.Memory = &v1.Memory{
					Guest: &guestMemory,
				}

				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(vmi)

				Expect(console.LoginToCirros(vmi)).To(Succeed())

				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "[ $(free -m | grep Mem: | tr -s ' ' | cut -d' ' -f2) -gt 200 ] && echo 'pass'\n"},
					&expect.BExp{R: console.RetValue("pass")},
					&expect.BSnd{S: "swapoff -a && dd if=/dev/zero of=/dev/shm/test bs=1k count=100k\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: "echo $?\n"},
					&expect.BExp{R: console.RetValue("0")},
				}, 15)).To(Succeed())

				pod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())

				podMemoryUsage, err := getPodMemoryUsage(pod)
				Expect(err).ToNot(HaveOccurred())
				By("Converting pod memory usage")
				m, err := strconv.Atoi(strings.Trim(podMemoryUsage, "\n"))
				Expect(err).ToNot(HaveOccurred())
				By("Checking if pod memory usage is > 64Mi")
				Expect(m).To(BeNumerically(">", 67108864), "67108864 B = 64 Mi")
			})

		})

		Context("[rfe_id:609][crit:medium][vendor:cnv-qe@redhat.com][level:component]Support memory over commitment test", func() {

			startOverCommitGuestOverheadVMI := func() (*v1.VirtualMachineInstance, error) {
				vmi := libvmifact.NewCirros(overcommitGuestOverhead())
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				if err != nil {
					return nil, err
				}
				return libwait.WaitForSuccessfulVMIStart(vmi), nil
			}

			It("[test_id:730]Check OverCommit VM Created and Started", func() {
				overcommitVmi, err := startOverCommitGuestOverheadVMI()
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(overcommitVmi)
			})
			It("[test_id:731]Check OverCommit status on VMI", func() {
				overcommitVmi, err := startOverCommitGuestOverheadVMI()
				Expect(err).ToNot(HaveOccurred())
				Expect(overcommitVmi.Spec.Domain.Resources.OvercommitGuestOverhead).To(BeTrue())
			})
			It("[test_id:732]Check Free memory on the VMI", func() {
				overcommitVmi, err := startOverCommitGuestOverheadVMI()
				Expect(err).ToNot(HaveOccurred())
				By("Expecting console")
				Expect(console.LoginToCirros(overcommitVmi)).To(Succeed())

				// Check on the VM, if the Free memory is roughly what we expected
				Expect(console.SafeExpectBatch(overcommitVmi, []expect.Batcher{
					&expect.BSnd{S: "[ $(free -m | grep Mem: | tr -s ' ' | cut -d' ' -f2) -gt 90 ] && echo 'pass'\n"},
					&expect.BExp{R: console.RetValue("pass")},
				}, 15)).To(Succeed())
			})
		})

		Context("[rfe_id:3078][crit:medium][vendor:cnv-qe@redhat.com][level:component]with usb controller", func() {
			It("[test_id:3117]should start the VMI with usb controller when usb device is present", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithTablet("tablet0", "usb"),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of usb under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "ls -l /sys/bus/usb/devices/usb* | wc -l\n"},
					&expect.BExp{R: console.RetValue("2")},
				}, 60)).To(Succeed(), "should report number of usb")
			})

			It("[test_id:3117]should start the VMI with usb controller when input device doesn't have bus", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithTablet("tablet0", ""),
				)
				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of usb under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "ls -l /sys/bus/usb/devices/usb* | wc -l\n"},
					&expect.BExp{R: console.RetValue("2")},
				}, 60)).To(Succeed(), "should report number of usb")
			})

			It("[test_id:3118]should start the VMI without usb controller", func() {
				vmi := libvmifact.NewAlpine()
				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")

				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the number of usb under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "ls -l /sys/bus/usb/devices/usb* 2>/dev/null | wc -l\n"},
					&expect.BExp{R: console.RetValue("0")},
				}, 60)).To(Succeed(), "should report number of usb")
			})
		})

		Context("[rfe_id:3077][crit:medium][vendor:cnv-qe@redhat.com][level:component]with input devices", func() {
			It("[test_id:2642]should failed to start the VMI with wrong type of input device", func() {
				vmi := libvmifact.NewCirros()
				vmi.Spec.Domain.Devices.Inputs = []v1.Input{
					{
						Name: "tablet0",
						Type: "keyboard",
						Bus:  v1.VirtIO,
					},
				}
				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred(), "should not start vmi")
			})

			It("[test_id:3074]should failed to start the VMI with wrong bus of input device", func() {
				vmi := libvmifact.NewCirros(
					libvmi.WithTablet("tablet0", "ps2"),
				)
				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred(), "should not start vmi")
			})

			It("[test_id:3072]should start the VMI with tablet input device with virtio bus", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithTablet("tablet0", "virtio"),
				)
				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the tablet input under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep -rs '^QEMU Virtio Tablet' /sys/devices | wc -l\n"},
					&expect.BExp{R: console.RetValue("1")},
				}, 60)).To(Succeed(), "should report input device")
			})

			It("[test_id:3073]should start the VMI with tablet input device with usb bus", func() {
				vmi := libvmifact.NewAlpine(
					libvmi.WithTablet("tablet0", "usb"),
				)

				By("Starting a VirtualMachineInstance")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "should start vmi")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(vmi)).To(Succeed())

				By("Checking the tablet input under guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "grep -rs '^QEMU USB Tablet' /sys/devices | wc -l\n"},
					&expect.BExp{R: console.RetValue("1")},
				}, 60)).To(Succeed(), "should report input device")
			})
		})

		Context("with namespace different from provided", func() {
			It("should fail admission", func() {
				// create a namespace default limit
				limitRangeObj := k8sv1.LimitRange{

					ObjectMeta: metav1.ObjectMeta{Name: "abc1", Namespace: testsuite.GetTestNamespace(nil)},
					Spec: k8sv1.LimitRangeSpec{
						Limits: []k8sv1.LimitRangeItem{
							{
								Type: k8sv1.LimitTypeContainer,
								Default: k8sv1.ResourceList{
									k8sv1.ResourceCPU:    resource.MustParse("2000m"),
									k8sv1.ResourceMemory: resource.MustParse("512M"),
								},
								DefaultRequest: k8sv1.ResourceList{
									k8sv1.ResourceCPU: resource.MustParse("500m"),
								},
							},
						},
					},
				}
				_, err := virtClient.CoreV1().LimitRanges(testsuite.GetTestNamespace(nil)).Create(context.Background(), &limitRangeObj, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				vmi := libvmifact.NewAlpine()
				vmi.Namespace = testsuite.NamespaceTestAlternative
				vmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: k8sv1.ResourceList{
						k8sv1.ResourceCPU: resource.MustParse("1000m"),
					},
				}

				By("Creating a VMI")
				Consistently(func() error {
					_, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), vmi, metav1.CreateOptions{})
					return err
				}, 30*time.Second, time.Second).Should(And(HaveOccurred(), MatchError("the namespace of the provided object does not match the namespace sent on the request")))
			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with hugepages", func() {
			verifyHugepagesConsumption := func(hugepagesVmi *v1.VirtualMachineInstance) bool {
				vmiPod, err := libpod.GetPodByVirtualMachineInstance(hugepagesVmi, testsuite.GetTestNamespace(hugepagesVmi))
				Expect(err).ToNot(HaveOccurred())

				hugepagesSize := resource.MustParse(hugepagesVmi.Spec.Domain.Memory.Hugepages.PageSize)
				hugepagesDir := fmt.Sprintf("/sys/kernel/mm/hugepages/hugepages-%dkB", hugepagesSize.Value()/int64(1024))

				// Get a hugepages statistics from virt-launcher pod
				output, err := exec.ExecuteCommandOnPod(
					vmiPod,
					vmiPod.Spec.Containers[0].Name,
					[]string{"cat", fmt.Sprintf("%s/nr_hugepages", hugepagesDir)},
				)
				Expect(err).ToNot(HaveOccurred())

				totalHugepages, err := strconv.Atoi(strings.Trim(output, "\n"))
				Expect(err).ToNot(HaveOccurred())

				output, err = exec.ExecuteCommandOnPod(
					vmiPod,
					vmiPod.Spec.Containers[0].Name,
					[]string{"cat", fmt.Sprintf("%s/free_hugepages", hugepagesDir)},
				)
				Expect(err).ToNot(HaveOccurred())

				freeHugepages, err := strconv.Atoi(strings.Trim(output, "\n"))
				Expect(err).ToNot(HaveOccurred())

				output, err = exec.ExecuteCommandOnPod(
					vmiPod,
					vmiPod.Spec.Containers[0].Name,
					[]string{"cat", fmt.Sprintf("%s/resv_hugepages", hugepagesDir)},
				)
				Expect(err).ToNot(HaveOccurred())

				resvHugepages, err := strconv.Atoi(strings.Trim(output, "\n"))
				Expect(err).ToNot(HaveOccurred())

				// Verify that the VM memory equals to a number of consumed hugepages
				vmHugepagesConsumption := int64(totalHugepages-freeHugepages+resvHugepages) * hugepagesSize.Value()
				vmMemory := hugepagesVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceMemory]
				if hugepagesVmi.Spec.Domain.Memory != nil && hugepagesVmi.Spec.Domain.Memory.Guest != nil {
					vmMemory = *hugepagesVmi.Spec.Domain.Memory.Guest
				}

				if vmHugepagesConsumption == vmMemory.Value() {
					return true
				}
				return false
			}

			noGuestOption := func() libvmi.Option {
				return func(vmi *v1.VirtualMachineInstance) {
					vmi.Spec.Domain.Memory.Guest = nil
				}
			}

			DescribeTable("should consume hugepages ", func(options ...libvmi.Option) {
				hugepagesVmi := libvmifact.NewCirros(options...)

				By("Starting a VM")
				hugepagesVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), hugepagesVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(hugepagesVmi)

				By("Checking that the VM memory equals to a number of consumed hugepages")
				Eventually(func() bool { return verifyHugepagesConsumption(hugepagesVmi) }, 30*time.Second, 5*time.Second).Should(BeTrue())
			},
				Entry("[test_id:1671]hugepages-2Mi", decorators.RequiresHugepages2Mi, Serial, libvmi.WithHugepages("2Mi"), libvmi.WithMemoryRequest("64Mi"), noGuestOption()),
				Entry("[test_id:1672]hugepages-1Gi", decorators.RequiresHugepages1Gi, Serial, libvmi.WithHugepages("1Gi"), libvmi.WithMemoryRequest("1Gi"), noGuestOption()),
				Entry("[test_id:1672]hugepages-2Mi with guest memory set explicitly", decorators.RequiresHugepages2Mi, Serial, libvmi.WithHugepages("2Mi"), libvmi.WithMemoryRequest("70Mi"), libvmi.WithGuestMemory("64Mi")),
			)

			Context("with unsupported page size", func() {
				It("[test_id:1673]should failed to schedule the pod", func() {
					hugepagesVmi := libvmifact.NewCirros(
						libvmi.WithMemoryRequest("66Mi"),
						libvmi.WithHugepages("3Mi"),
					)

					By("Starting a VM")
					hugepagesVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(hugepagesVmi)).Create(context.Background(), hugepagesVmi, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred())

					var vmiCondition v1.VirtualMachineInstanceCondition
					Eventually(func() bool {
						vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(hugepagesVmi)).Get(context.Background(), hugepagesVmi.Name, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())

						for _, cond := range vmi.Status.Conditions {
							if cond.Type == v1.VirtualMachineInstanceConditionType(k8sv1.PodScheduled) && cond.Status == k8sv1.ConditionFalse {
								vmiCondition = cond
								return true
							}
						}
						return false
					}, 30*time.Second, time.Second).Should(BeTrue())
					Expect(vmiCondition.Message).To(ContainSubstring("Insufficient hugepages-3Mi"))
					Expect(vmiCondition.Reason).To(Equal("Unschedulable"))
				})
			})
		})

		Context("[rfe_id:893][crit:medium][vendor:cnv-qe@redhat.com][level:component]with rng", func() {
			It("[test_id:1674]should have the virtio rng device present when present", func() {
				rngVmi := libvmifact.NewAlpine(libvmi.WithRng())

				By("Starting a VirtualMachineInstance")
				rngVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(rngVmi)).Create(context.Background(), rngVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(rngVmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(rngVmi)).To(Succeed())

				By("Checking the virtio rng presence")
				Expect(console.SafeExpectBatch(rngVmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^virtio /sys/devices/virtual/misc/hw_random/rng_available\n"},
					&expect.BExp{R: console.RetValue("1")},
				}, 400)).To(Succeed())
			})

			It("[test_id:1675]should not have the virtio rng device when not present", func() {
				By("Starting a VirtualMachineInstance")
				rngVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), libvmifact.NewAlpine(withNoRng()), metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(rngVmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToAlpine(rngVmi)).To(Succeed())

				By("Checking the virtio rng presence")
				Expect(console.SafeExpectBatch(rngVmi, []expect.Batcher{
					&expect.BSnd{S: "[[ ! -e /sys/devices/virtual/misc/hw_random/rng_available ]] && echo non\n"},
					&expect.BExp{R: console.RetValue("non")},
				}, 400)).To(Succeed())
			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with guestAgent", func() {
			prepareAgentVM := func() *v1.VirtualMachineInstance {
				agentVMI := libvmifact.NewFedora(libnet.WithMasqueradeNetworking())

				By("Starting a VirtualMachineInstance")
				agentVMI, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Create(context.Background(), agentVMI, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "Should create VMI successfully")
				libwait.WaitForSuccessfulVMIStart(agentVMI)

				getOptions := metav1.GetOptions{}
				var freshVMI *v1.VirtualMachineInstance

				By("VMI has the guest agent connected condition")
				Eventually(func() []v1.VirtualMachineInstanceCondition {
					freshVMI, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Get(context.Background(), agentVMI.Name, getOptions)
					Expect(err).ToNot(HaveOccurred(), "Should get VMI ")
					return freshVMI.Status.Conditions
				}, 240*time.Second, 2).Should(
					ContainElement(
						MatchFields(
							IgnoreExtras,
							Fields{"Type": Equal(v1.VirtualMachineInstanceAgentConnected)})),
					"Should have agent connected condition")

				return agentVMI
			}

			It("[test_id:1676]should have attached a guest agent channel by default", func() {
				agentVMI := libvmifact.NewAlpine()
				By("Starting a VirtualMachineInstance")
				agentVMI, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Create(context.Background(), agentVMI, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "Should create VMI successfully")
				libwait.WaitForSuccessfulVMIStart(agentVMI)

				getOptions := metav1.GetOptions{}
				var freshVMI *v1.VirtualMachineInstance

				freshVMI, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Get(context.Background(), agentVMI.Name, getOptions)
				Expect(err).ToNot(HaveOccurred(), "Should get VMI ")

				domXML, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, freshVMI)
				Expect(err).ToNot(HaveOccurred(), "Should return XML from VMI")

				Expect(domXML).To(ContainSubstring("<channel type='unix'>"), "Should contain at least one channel")
				Expect(domXML).To(ContainSubstring("<target type='virtio' name='org.qemu.guest_agent.0' state='disconnected'/>"), "Should have guest agent channel present")
				Expect(domXML).To(ContainSubstring("<alias name='channel0'/>"), "Should have guest channel present")
			})

			It("[test_id:1677]VMI condition should signal agent presence", func() {
				agentVMI := prepareAgentVM()
				getOptions := metav1.GetOptions{}

				freshVMI, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Get(context.Background(), agentVMI.Name, getOptions)
				Expect(err).ToNot(HaveOccurred(), "Should get VMI ")
				Expect(freshVMI.Status.Conditions).To(
					ContainElement(
						MatchFields(
							IgnoreExtras,
							Fields{"Type": Equal(v1.VirtualMachineInstanceAgentConnected)})),
					"agent should already be connected")

			})

			It("[test_id:4625]should remove condition when agent is off", func() {
				agentVMI := prepareAgentVM()
				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(agentVMI)).To(Succeed())

				By("Terminating guest agent and waiting for it to disappear.")
				Expect(console.SafeExpectBatch(agentVMI, []expect.Batcher{
					&expect.BSnd{S: "systemctl stop qemu-guest-agent\n"},
					&expect.BExp{R: console.PromptExpression},
				}, 400)).To(Succeed())

				By("VMI has the guest agent connected condition")
				Eventually(matcher.ThisVMI(agentVMI), 240*time.Second, 2).Should(matcher.HaveConditionMissingOrFalse(v1.VirtualMachineInstanceAgentConnected))
			})

			Context("with cluster config changes", Serial, func() {
				BeforeEach(func() {
					kv := libkubevirt.GetCurrentKv(virtClient)

					config := kv.Spec.Configuration
					config.SupportedGuestAgentVersions = []string{"X.*"}
					kvconfig.UpdateKubeVirtConfigValueAndWait(config)
				})

				It("[test_id:5267]VMI condition should signal unsupported agent presence", func() {
					agentVMI := libvmifact.NewFedora(
						libnet.WithMasqueradeNetworking(),
						libvmi.WithCloudInitNoCloud(
							libvmici.WithNoCloudUserData(cloudinit.GetFedoraToolsGuestAgentBlacklistUserData("guest-shutdown")),
						),
					)
					By("Starting a VirtualMachineInstance")
					agentVMI, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Create(context.Background(), agentVMI, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred(), "Should create VMI successfully")
					libwait.WaitForSuccessfulVMIStart(agentVMI)

					Eventually(matcher.ThisVMI(agentVMI), 240*time.Second, 2*time.Second).Should(matcher.HaveConditionTrue(v1.VirtualMachineInstanceUnsupportedAgent))
				})

				It("[test_id:6958]VMI condition should not signal unsupported agent presence for optional commands", func() {
					agentVMI := libvmifact.NewFedora(
						libnet.WithMasqueradeNetworking(),
						libvmi.WithCloudInitNoCloud(
							libvmici.WithNoCloudUserData(cloudinit.GetFedoraToolsGuestAgentBlacklistUserData("guest-exec,guest-set-password")),
						),
					)
					By("Starting a VirtualMachineInstance")
					agentVMI, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Create(context.Background(), agentVMI, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred(), "Should create VMI successfully")
					libwait.WaitForSuccessfulVMIStart(agentVMI)

					By("VMI has the guest agent connected condition")
					Eventually(matcher.ThisVMI(agentVMI), 240*time.Second, 2*time.Second).Should(matcher.HaveConditionTrue(v1.VirtualMachineInstanceAgentConnected))

					By("fetching the VMI after agent has connected")
					Expect(matcher.ThisVMI(agentVMI)()).To(matcher.HaveConditionMissingOrFalse(v1.VirtualMachineInstanceUnsupportedAgent))
				})
			})

			It("[test_id:4626]should have guestosinfo in status when agent is present", func() {
				agentVMI := prepareAgentVM()
				getOptions := metav1.GetOptions{}
				var updatedVmi *v1.VirtualMachineInstance
				var err error

				By("Expecting the Guest VM information")
				Eventually(func() bool {
					updatedVmi, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).Get(context.Background(), agentVMI.Name, getOptions)
					if err != nil {
						return false
					}
					return updatedVmi.Status.GuestOSInfo.Name != ""
				}, 240*time.Second, 2).Should(BeTrue(), "Should have guest OS Info in vmi status")

				Expect(err).ToNot(HaveOccurred())
				Expect(updatedVmi.Status.GuestOSInfo.Name).To(ContainSubstring("Fedora"))
			})

			It("[test_id:4627]should return the whole data when agent is present", func() {
				agentVMI := prepareAgentVM()

				By("Expecting the Guest VM information")
				Eventually(func() bool {
					guestInfo, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).GuestOsInfo(context.Background(), agentVMI.Name)
					if err != nil {
						// invalid request, retry
						return false
					}

					return guestInfo.Hostname != "" &&
						guestInfo.Timezone != "" &&
						guestInfo.GAVersion != "" &&
						guestInfo.OS.Name != "" &&
						len(guestInfo.FSInfo.Filesystems) > 0

				}, 240*time.Second, 2).Should(BeTrue(), "Should have guest OS Info in subresource")
			})

			It("[test_id:4628]should not return the whole data when agent is not present", func() {
				agentVMI := prepareAgentVM()

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(agentVMI)).To(Succeed())

				By("Terminating guest agent and waiting for it to disappear.")
				Expect(console.SafeExpectBatch(agentVMI, []expect.Batcher{
					&expect.BSnd{S: "systemctl stop qemu-guest-agent\n"},
					&expect.BExp{R: console.PromptExpression},
				}, 400)).To(Succeed())

				By("Expecting the Guest VM information")
				Eventually(func() string {
					_, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).GuestOsInfo(context.Background(), agentVMI.Name)
					if err != nil {
						return err.Error()
					}
					return ""
				}, 240*time.Second, 2).Should(ContainSubstring("VMI does not have guest agent connected"), "Should have not have guest info in subresource")
			})

			It("[test_id:4629]should return user list", func() {
				agentVMI := prepareAgentVM()

				Expect(console.LoginToFedora(agentVMI)).To(Succeed())

				By("Expecting the Guest VM information")
				Eventually(func() bool {
					userList, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).UserList(context.Background(), agentVMI.Name)
					if err != nil {
						// invalid request, retry
						return false
					}

					return len(userList.Items) > 0 && userList.Items[0].UserName == "fedora"

				}, 240*time.Second, 2).Should(BeTrue(), "Should have fedora users")
			})

			It("[test_id:4630]should return filesystem list", func() {
				agentVMI := prepareAgentVM()

				By("Expecting the Guest VM information")
				Eventually(func() bool {
					fsList, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(agentVMI)).FilesystemList(context.Background(), agentVMI.Name)
					if err != nil {
						// invalid request, retry
						return false
					}

					return len(fsList.Items) > 0 && fsList.Items[0].DiskName != "" && fsList.Items[0].MountPoint != "" &&
						len(fsList.Items[0].Disk) > 0 && fsList.Items[0].Disk[0].BusType != ""

				}, 240*time.Second, 2).Should(BeTrue(), "Should have some filesystem")
			})

		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with serial-number", func() {

			It("[test_id:3121]should have serial-number set when present", func() {
				const serial = "4b2f5496-f3a3 460b-a375-168223f68845"
				snVmi := libvmifact.NewAlpine()
				snVmi.Spec.Domain.Firmware = &v1.Firmware{Serial: serial}

				By("Starting a VirtualMachineInstance")
				snVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(snVmi)).Create(context.Background(), snVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(snVmi)
				Expect(console.LoginToAlpine(snVmi)).To(Succeed())

				Expect(console.SafeExpectBatch(snVmi, []expect.Batcher{
					&expect.BSnd{S: "cat /sys/devices/virtual/dmi/id/subsystem/id/product_serial\n"},
					&expect.BExp{R: serial},
				}, 15)).To(Succeed(), "should report the configured serial numnber")
			})
		})

		Context("with TSC timer", func() {
			featureSupportedInAtLeastOneNode := func(nodes *k8sv1.NodeList, feature string) bool {
				for _, node := range nodes.Items {
					for label := range node.Labels {
						if strings.Contains(label, v1.CPUFeatureLabel) && strings.Contains(label, feature) {
							return true
						}
					}
				}
				return false
			}
			It("[test_id:6843]should set a TSC frequency and have the CPU flag available in the guest", decorators.Invtsc, decorators.TscFrequencies, func() {
				nodes := libnode.GetAllSchedulableNodes(virtClient)
				Expect(featureSupportedInAtLeastOneNode(nodes, "invtsc")).To(BeTrue(), "To run this test at least one node should support invtsc feature")
				vmi := libvmifact.NewCirros()
				vmi.Spec.Domain.CPU = &v1.CPU{
					Features: []v1.CPUFeature{
						{
							Name:   "invtsc",
							Policy: "require",
						},
					},
				}
				By("Expecting the VirtualMachineInstance start")
				vmi = libvmops.RunVMIAndExpectLaunch(vmi, 180)

				By("Checking the TSC frequency on the VMI")
				vmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Status.TopologyHints).ToNot(BeNil())
				Expect(vmi.Status.TopologyHints.TSCFrequency).ToNot(BeNil())

				By("Checking the TSC frequency on the Domain XML")
				domainSpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
				Expect(err).ToNot(HaveOccurred())
				timerFrequency := ""
				for _, timer := range domainSpec.Clock.Timer {
					if timer.Name == "tsc" {
						timerFrequency = timer.Frequency
					}
				}
				Expect(timerFrequency).ToNot(BeEmpty())

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(vmi)).To(Succeed())

				By("Checking the CPU model under the guest OS")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: fmt.Sprintf("grep '%s' /proc/cpuinfo > /dev/null\n", "nonstop_tsc")},
					&expect.BExp{R: fmt.Sprintf(console.PromptExpression)},
					&expect.BSnd{S: "echo $?\n"},
					&expect.BExp{R: console.RetValue("0")},
				}, 10)).To(Succeed())
			})
		})

		Context("with Clock and timezone", func() {

			It("[sig-compute][test_id:5268]guest should see timezone", func() {
				vmi := libvmifact.NewCirros()
				timezone := "America/New_York"
				tz := v1.ClockOffsetTimezone(timezone)
				vmi.Spec.Domain.Clock = &v1.Clock{
					ClockOffset: v1.ClockOffset{
						Timezone: &tz,
					},
					Timer: &v1.Timer{},
				}

				By("Creating a VMI with timezone set")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Waiting for successful start of VMI")
				libwait.WaitForSuccessfulVMIStart(vmi)

				By("Logging to VMI")
				Expect(console.LoginToCirros(vmi)).To(Succeed())

				loc, err := time.LoadLocation(timezone)
				Expect(err).ToNot(HaveOccurred())
				now := time.Now().In(loc)
				nowplus := now.Add(20 * time.Second)
				nowminus := now.Add(-20 * time.Second)
				By("Checking hardware clock time")
				expected := fmt.Sprintf("(%02d:%02d:|%02d:%02d:|%02d:%02d:)", nowminus.Hour(), nowminus.Minute(), now.Hour(), now.Minute(), nowplus.Hour(), nowplus.Minute())
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "sudo hwclock --localtime \n"},
					&expect.BExp{R: expected},
				}, 20)).To(Succeed(), "Expected the VM time to be within 20 seconds of "+now.String())

			})
		})

		Context("with volumes, disks and filesystem defined", func() {

			It("[test_id:6960]should reject disk with missing volume", func() {
				vmi := libvmifact.NewGuestless()
				const diskName = "testdisk"
				vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
					Name: diskName,
				})
				_, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
				const expectedErrMessage = "denied the request: spec.domain.devices.disks[0].Name '" + diskName + "' not found."
				Expect(err.Error()).To(ContainSubstring(expectedErrMessage))
			})
		})

		Context("using defaultRuntimeClass configuration", Serial, func() {
			var runtimeClassName string

			BeforeEach(func() {
				// use random runtime class to avoid collisions with cleanup where a
				// runtime class is still in the process of being deleted because pod
				// cleanup is still in progress
				runtimeClassName = "fake-runtime-class" + "-" + rand.String(5)
				By("Creating a runtime class")
				Expect(createRuntimeClass(runtimeClassName, "fake-handler")).To(Succeed())
			})

			AfterEach(func() {
				By("Cleaning up runtime class")
				Expect(deleteRuntimeClass(runtimeClassName)).To(Succeed())
			})

			It("should apply runtimeClassName to pod when set", func() {
				By("Configuring a default runtime class")
				config := libkubevirt.GetCurrentKv(virtClient).Spec.Configuration.DeepCopy()
				config.DefaultRuntimeClass = runtimeClassName
				kvconfig.UpdateKubeVirtConfigValueAndWait(*config)

				By("Creating a new VMI")
				vmi := libvmifact.NewGuestless()
				// Runtime class related warnings are expected since we created a fake runtime class that isn't supported
				wp := watcher.WarningsPolicy{FailOnWarnings: true, WarningsIgnoreList: []string{"RuntimeClass"}}
				vmi = libvmops.RunVMIAndExpectSchedulingWithWarningPolicy(vmi, 30, wp)

				By("Checking for presence of runtimeClassName")
				pod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())
				Expect(pod.Spec.RuntimeClassName).ToNot(BeNil())
				Expect(*pod.Spec.RuntimeClassName).To(BeEquivalentTo(runtimeClassName))
			})
		})
		It("should not apply runtimeClassName to pod when not set", func() {
			By("verifying no default runtime class name is set")
			config := libkubevirt.GetCurrentKv(virtClient).Spec.Configuration
			Expect(config.DefaultRuntimeClass).To(BeEmpty())
			By("Creating a VMI")
			vmi := libvmops.RunVMIAndExpectLaunch(libvmifact.NewGuestless(), 60)

			By("Checking for absence of runtimeClassName")
			pod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Spec.RuntimeClassName).To(BeNil())
		})

		Context("with geust-to-request memory ", Serial, func() {
			setHeadroom := func(ratioStr string) {
				kv := libkubevirt.GetCurrentKv(virtClient)

				config := kv.Spec.Configuration
				config.AdditionalGuestMemoryOverheadRatio = &ratioStr
				kvconfig.UpdateKubeVirtConfigValueAndWait(config)
			}

			getComputeMemoryRequest := func(vmi *v1.VirtualMachineInstance) resource.Quantity {
				launcherPod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())
				computeContainer := libpod.LookupComputeContainer(launcherPod)
				return computeContainer.Resources.Requests[k8sv1.ResourceMemory]
			}

			It("should add guest-to-memory headroom", func() {
				const guestMemoryStr = "1024M"
				origVmiWithoutHeadroom := libvmi.New(
					libvmi.WithMemoryRequest(guestMemoryStr),
					libvmi.WithGuestMemory(guestMemoryStr),
				)
				origVmiWithHeadroom := libvmi.New(
					libvmi.WithMemoryRequest(guestMemoryStr),
					libvmi.WithGuestMemory(guestMemoryStr),
				)

				By("Running a vmi without additional headroom")
				vmiWithoutHeadroom := libvmops.RunVMIAndExpectScheduling(origVmiWithoutHeadroom, 60)

				By("Setting a headroom ratio in Kubevirt CR")
				const ratio = "1.567"
				setHeadroom(ratio)

				By("Running a vmi with additional headroom")
				vmiWithHeadroom := libvmops.RunVMIAndExpectScheduling(origVmiWithHeadroom, 60)

				requestWithoutHeadroom := getComputeMemoryRequest(vmiWithoutHeadroom)
				requestWithHeadroom := getComputeMemoryRequest(vmiWithHeadroom)

				overheadWithoutHeadroom := services.GetMemoryOverhead(vmiWithoutHeadroom, runtime.GOARCH, nil)
				overheadWithHeadroom := services.GetMemoryOverhead(vmiWithoutHeadroom, runtime.GOARCH, pointer.P(ratio))

				expectedDiffBetweenRequests := overheadWithHeadroom.DeepCopy()
				expectedDiffBetweenRequests.Sub(overheadWithoutHeadroom)

				actualDiffBetweenRequests := requestWithHeadroom.DeepCopy()
				actualDiffBetweenRequests.Sub(requestWithoutHeadroom)

				By("Ensuring memory request is as expected")
				const errFmt = "ratio: %s, request without headroom: %s, request with headroom: %s, overhead without headroom: %s, overhead with headroom: %s, expected diff between requests: %s, actual diff between requests: %s"
				Expect(actualDiffBetweenRequests.Cmp(expectedDiffBetweenRequests)).To(Equal(0),
					fmt.Sprintf(errFmt, ratio, requestWithoutHeadroom.String(), requestWithHeadroom.String(), overheadWithoutHeadroom.String(), overheadWithHeadroom.String(), expectedDiffBetweenRequests.String(), actualDiffBetweenRequests.String()))

				By("Ensure no memory specifications had been changed on VMIs")
				Expect(origVmiWithHeadroom.Spec.Domain.Resources).To(Equal(vmiWithHeadroom.Spec.Domain.Resources), "vmi resources are not expected to change")
				Expect(origVmiWithHeadroom.Spec.Domain.Memory).To(Equal(vmiWithHeadroom.Spec.Domain.Memory), "vmi guest memory is not expected to change")
				Expect(origVmiWithoutHeadroom.Spec.Domain.Resources).To(Equal(vmiWithoutHeadroom.Spec.Domain.Resources), "vmi resources are not expected to change")
				Expect(origVmiWithoutHeadroom.Spec.Domain.Memory).To(Equal(vmiWithoutHeadroom.Spec.Domain.Memory), "vmi guest memory is not expected to change")
			})
		})
	})

	Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with CPU spec", func() {
		var nodes *k8sv1.NodeList

		parseCPUNiceName := func(name string) string {
			updatedCPUName := strings.Replace(name, "\n", "", -1)
			if strings.Contains(updatedCPUName, ":") {
				updatedCPUName = strings.Split(name, ":")[1]

			}
			updatedCPUName = strings.Replace(updatedCPUName, " ", "", 1)
			updatedCPUName = strings.Replace(updatedCPUName, "(", "", -1)
			updatedCPUName = strings.Replace(updatedCPUName, ")", "", -1)

			updatedCPUName = strings.Split(updatedCPUName, "-")[0]
			updatedCPUName = strings.Split(updatedCPUName, "_")[0]

			for i, char := range updatedCPUName {
				if unicode.IsUpper(char) && i != 0 {
					updatedCPUName = strings.Split(updatedCPUName, string(char))[0]
				}
			}
			return updatedCPUName
		}

		BeforeEach(func() {
			nodes = libnode.GetAllSchedulableNodes(virtClient)
			Expect(nodes.Items).ToNot(BeEmpty(), "There should be some compute node")
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]when CPU model defined", func() {
			It("[test_id:1678]should report defined CPU model", func() {
				supportedCPUs := libnode.GetSupportedCPUModels(*nodes)
				Expect(supportedCPUs).ToNot(BeEmpty())
				cpuVmi := libvmifact.NewCirros(libvmi.WithCPUModel(supportedCPUs[0]))

				niceName := parseCPUNiceName(supportedCPUs[0])

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(cpuVmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the CPU model under the guest OS")
				Expect(console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: fmt.Sprintf("grep %s /proc/cpuinfo\n", niceName)},
					&expect.BExp{R: fmt.Sprintf(".*model name.*%s.*", niceName)},
				}, 10)).To(Succeed())
			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]when CPU model equals to passthrough", func() {
			It("[test_id:1679]should report exactly the same model as node CPU", func() {
				cpuVmi := libvmifact.NewCirros(libvmi.WithCPUModel("host-passthrough"))

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(cpuVmi)

				By("Checking the CPU model under the guest OS")
				output := libpod.RunCommandOnVmiPod(cpuVmi, []string{"grep", "-m1", "model name", "/proc/cpuinfo"})

				niceName := parseCPUNiceName(output)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the CPU model under the guest OS")
				Expect(console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: fmt.Sprintf("grep '%s' /proc/cpuinfo\n", niceName)},
					&expect.BExp{R: fmt.Sprintf(".*model name.*%s.*", niceName)},
				}, 10)).To(Succeed())
			})
		})

		Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]when CPU model not defined", func() {
			It("[test_id:1680]should report CPU model from libvirt capabilities", func() {
				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), libvmifact.NewCirros(), metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(cpuVmi)

				output := libpod.RunCommandOnVmiPod(cpuVmi, []string{"grep", "-m1", "model name", "/proc/cpuinfo"})

				niceName := parseCPUNiceName(output)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the CPU model under the guest OS")
				console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: fmt.Sprintf("grep '%s' /proc/cpuinfo\n", niceName)},
					&expect.BExp{R: fmt.Sprintf(".*model name.*%s.*", niceName)},
				}, 10)
			})
		})

		Context("when CPU features defined", func() {
			It("[test_id:3123]should start a Virtual Machine with matching features", func() {
				supportedCPUFeatures := libnode.GetSupportedCPUFeatures(*nodes)
				Expect(supportedCPUFeatures).ToNot(BeEmpty())
				cpuVmi := libvmifact.NewCirros(libvmi.WithCPUFeature(supportedCPUFeatures[0], ""))

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				libwait.WaitForSuccessfulVMIStart(cpuVmi)

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())
			})
		})
	})

	Context("[rfe_id:2869][crit:medium][vendor:cnv-qe@redhat.com][level:component]with machine type settings", Serial, func() {
		testEmulatedMachines := []string{"q35*", "pc-q35*", "pc*"}

		BeforeEach(func() {
			kv := libkubevirt.GetCurrentKv(virtClient)

			config := kv.Spec.Configuration
			config.MachineType = ""
			config.ArchitectureConfiguration = &v1.ArchConfiguration{Amd64: &v1.ArchSpecificConfiguration{}, Arm64: &v1.ArchSpecificConfiguration{}, Ppc64le: &v1.ArchSpecificConfiguration{}, S390x: &v1.ArchSpecificConfiguration{}}
			config.ArchitectureConfiguration.Amd64.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.Arm64.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.Ppc64le.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.S390x.EmulatedMachines = testEmulatedMachines

			kvconfig.UpdateKubeVirtConfigValueAndWait(config)
		})

		It("[test_id:3124]should set machine type from VMI spec", func() {
			vmi := libvmi.New(
				libvmi.WithMemoryRequest(enoughMemForSafeBiosEmulation),
				withMachineType("pc"),
			)
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 30)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)

			Expect(err).ToNot(HaveOccurred())
			Expect(runningVMISpec.OS.Type.Machine).To(ContainSubstring("pc-i440"))

			Expect(vmi.Status.Machine).ToNot(BeNil())
			Expect(vmi.Status.Machine.Type).To(Equal(runningVMISpec.OS.Type.Machine))
		})

		It("[test_id:3125]should allow creating VM without Machine defined", func() {
			vmi := libvmifact.NewGuestless()
			vmi.Spec.Domain.Machine = nil
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 30)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)

			Expect(err).ToNot(HaveOccurred())
			Expect(runningVMISpec.OS.Type.Machine).To(ContainSubstring("q35"))
		})

		It("[test_id:6964]should allow creating VM defined with Machine with an empty Type", func() {
			// This is needed to provide backward compatibility since our example VMIs used to be defined in this way
			vmi := libvmi.New(
				libvmi.WithMemoryRequest(enoughMemForSafeBiosEmulation),
				withMachineType(""),
			)

			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 30)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)

			Expect(err).ToNot(HaveOccurred())
			Expect(runningVMISpec.OS.Type.Machine).To(ContainSubstring("q35"))
		})

		It("[test_id:3126]should set machine type from kubevirt-config", Serial, func() {
			kv := libkubevirt.GetCurrentKv(virtClient)
			testEmulatedMachines := []string{"pc"}

			config := kv.Spec.Configuration

			config.ArchitectureConfiguration = &v1.ArchConfiguration{Amd64: &v1.ArchSpecificConfiguration{}, Arm64: &v1.ArchSpecificConfiguration{}, Ppc64le: &v1.ArchSpecificConfiguration{}, S390x: &v1.ArchSpecificConfiguration{}}
			config.ArchitectureConfiguration.Amd64.MachineType = "pc"
			config.ArchitectureConfiguration.Arm64.MachineType = "pc"
			config.ArchitectureConfiguration.Ppc64le.MachineType = "pc"
			config.ArchitectureConfiguration.S390x.MachineType = "pc"
			config.ArchitectureConfiguration.Amd64.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.Arm64.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.Ppc64le.EmulatedMachines = testEmulatedMachines
			config.ArchitectureConfiguration.S390x.EmulatedMachines = testEmulatedMachines
			kvconfig.UpdateKubeVirtConfigValueAndWait(config)

			vmi := libvmifact.NewGuestless()
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 30)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)

			Expect(err).ToNot(HaveOccurred())
			Expect(runningVMISpec.OS.Type.Machine).To(ContainSubstring("pc-i440"))
		})
	})

	Context("with a custom scheduler", func() {
		It("[test_id:4631]should set the custom scheduler on the pod", func() {
			vmi := libvmi.New(
				libvmi.WithMemoryRequest(enoughMemForSafeBiosEmulation),
				WithSchedulerName("my-custom-scheduler"),
			)
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)
			launcherPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			Expect(launcherPod.Spec.SchedulerName).To(Equal("my-custom-scheduler"))
		})
	})

	Context("[rfe_id:140][crit:medium][vendor:cnv-qe@redhat.com][level:component]with CPU request settings", func() {

		It("[test_id:3127]should set CPU request from VMI spec", func() {
			vmi := libvmi.New(
				libvmi.WithMemoryRequest(enoughMemForSafeBiosEmulation),
				libvmi.WithCPURequest("500m"),
			)
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

			readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			computeContainer := libpod.LookupComputeContainer(readyPod)
			cpuRequest := computeContainer.Resources.Requests[k8sv1.ResourceCPU]
			Expect(cpuRequest.String()).To(Equal("500m"))
		})

		It("[test_id:3128]should set CPU request when it is not provided", func() {
			vmi := libvmifact.NewGuestless()
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

			readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			computeContainer := libpod.LookupComputeContainer(readyPod)
			cpuRequest := computeContainer.Resources.Requests[k8sv1.ResourceCPU]
			Expect(cpuRequest.String()).To(Equal("100m"))
		})

		It("[test_id:3129]should set CPU request from kubevirt-config", Serial, func() {
			kv := libkubevirt.GetCurrentKv(virtClient)

			config := kv.Spec.Configuration
			configureCPURequest := resource.MustParse("800m")
			config.CPURequest = &configureCPURequest
			kvconfig.UpdateKubeVirtConfigValueAndWait(config)

			vmi := libvmifact.NewGuestless()
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

			readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			computeContainer := libpod.LookupComputeContainer(readyPod)
			cpuRequest := computeContainer.Resources.Requests[k8sv1.ResourceCPU]
			Expect(cpuRequest.String()).To(Equal("800m"))
		})
	})

	Context("with automatic CPU limit configured in the CR", Serial, func() {
		const autoCPULimitLabel = "autocpulimit"
		BeforeEach(func() {
			By("Adding a label selector to the CR for auto CPU limit")
			kv := libkubevirt.GetCurrentKv(virtClient)
			config := kv.Spec.Configuration
			config.AutoCPULimitNamespaceLabelSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{autoCPULimitLabel: "true"},
			}
			kvconfig.UpdateKubeVirtConfigValueAndWait(config)
		})
		It("should not set a CPU limit if the namespace doesn't match the selector", func() {
			By("Creating a running VMI")
			vmi := libvmifact.NewGuestless()
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

			By("Ensuring no CPU limit is set")
			readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			computeContainer := libpod.LookupComputeContainer(readyPod)
			_, exists := computeContainer.Resources.Limits[k8sv1.ResourceCPU]
			Expect(exists).To(BeFalse(), "CPU limit set on the compute container when none was expected")
		})
		It("should set a CPU limit if the namespace matches the selector", func() {
			By("Creating a VMI object")
			vmi := libvmifact.NewGuestless()

			By("Adding the right label to VMI namespace")
			namespace, err := virtClient.CoreV1().Namespaces().Get(context.Background(), testsuite.GetTestNamespace(vmi), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			patchData := []byte(fmt.Sprintf(`{"metadata": { "labels": {"%s": "true"}}}`, autoCPULimitLabel))
			_, err = virtClient.CoreV1().Namespaces().Patch(context.Background(), namespace.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Starting the VMI")
			runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

			By("Ensuring the CPU limit is set to the correct value")
			readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
			Expect(err).ToNot(HaveOccurred())
			computeContainer := libpod.LookupComputeContainer(readyPod)
			limits, exists := computeContainer.Resources.Limits[k8sv1.ResourceCPU]
			Expect(exists).To(BeTrue(), "expected CPU limit not set on the compute container")
			Expect(limits.String()).To(Equal("1"))
		})
	})

	Context("using automatic resource limits", func() {

		When("there is no ResourceQuota with memory and cpu limits associated with the creation namespace", func() {
			It("[test_id:11215]should not automatically set memory limits in the virt-launcher pod", func() {
				vmi := libvmifact.NewCirros()
				By("Creating a running VMI")
				runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

				By("Ensuring no memory and cpu limits are set")
				readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
				Expect(err).ToNot(HaveOccurred())
				computeContainer := libpod.LookupComputeContainer(readyPod)
				_, exists := computeContainer.Resources.Limits[k8sv1.ResourceMemory]
				Expect(exists).To(BeFalse(), "Memory limits set on the compute container when none was expected")
				_, exists = computeContainer.Resources.Limits[k8sv1.ResourceCPU]
				Expect(exists).To(BeFalse(), "CPU limits set on the compute container when none was expected")
			})
		})

		When("a ResourceQuota with memory and cpu limits is associated to the creation namespace", func() {
			It("[test_id:11214]should set cpu and memory limit in the virt-launcher pod", func() {
				vmiRequest := resource.MustParse("256Mi")
				vmi := libvmifact.NewCirros(
					libvmi.WithMemoryRequest(vmiRequest.String()),
					libvmi.WithCPUCount(1, 1, 1),
				)

				vmiPodRequest := services.GetMemoryOverhead(vmi, runtime.GOARCH, nil)
				vmiPodRequest.Add(vmiRequest)
				value := int64(float64(vmiPodRequest.Value()) * services.DefaultMemoryLimitOverheadRatio)

				expectedLauncherMemLimits := resource.NewQuantity(value, vmiPodRequest.Format)
				expectedLauncherCPULimits := resource.MustParse("1")

				// Add a delta to not saturate the rq
				rqLimit := expectedLauncherMemLimits.DeepCopy()
				delta := resource.MustParse("100Mi")
				rqLimit.Add(delta)
				By("Creating a Resource Quota with memory limits")
				rq := &k8sv1.ResourceQuota{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:    testsuite.GetTestNamespace(nil),
						GenerateName: "test-quota",
					},
					Spec: k8sv1.ResourceQuotaSpec{
						Hard: k8sv1.ResourceList{
							k8sv1.ResourceLimitsMemory: resource.MustParse(rqLimit.String()),
							k8sv1.ResourceLimitsCPU:    resource.MustParse("1500m"),
						},
					},
				}
				_, err := virtClient.CoreV1().ResourceQuotas(testsuite.GetTestNamespace(nil)).Create(context.Background(), rq, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Starting the VMI")
				runningVMI := libvmops.RunVMIAndExpectScheduling(vmi, 30)

				By("Ensuring the memory and cpu limits are set to the correct values")
				readyPod, err := libpod.GetPodByVirtualMachineInstance(runningVMI, testsuite.GetTestNamespace(vmi))
				Expect(err).ToNot(HaveOccurred())
				computeContainer := libpod.LookupComputeContainer(readyPod)
				memLimits, exists := computeContainer.Resources.Limits[k8sv1.ResourceMemory]
				Expect(exists).To(BeTrue(), "expected memory limits set on the compute container")
				Expect(memLimits.Value()).To(BeEquivalentTo(expectedLauncherMemLimits.Value()))
				cpuLimits, exists := computeContainer.Resources.Limits[k8sv1.ResourceCPU]
				Expect(exists).To(BeTrue(), "expected cpu limits set on the compute container")
				Expect(cpuLimits.Value()).To(BeEquivalentTo(expectedLauncherCPULimits.Value()))
			})
		})
	})

	Context("[rfe_id:904][crit:medium][vendor:cnv-qe@redhat.com][level:component]with driver cache and io settings and PVC", decorators.SigStorage, decorators.StorageReq, func() {

		It("[test_id:1681]should set appropriate cache modes", decorators.HostDiskGate, func() {
			if !checks.HasFeature(featuregate.HostDiskGate) {
				Fail("Cluster has the HostDisk featuregate disabled, use skip for HostDiskGate")
			}

			vmi := libvmi.New(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
				libvmi.WithMemoryRequest("128Mi"),
				libvmi.WithContainerDisk("ephemeral-disk1", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
				libvmi.WithContainerDisk("ephemeral-disk2", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
				libvmi.WithContainerDisk("ephemeral-disk5", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
				libvmi.WithContainerDisk("ephemeral-disk3", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
				libvmi.WithCloudInitNoCloud(libvmici.WithNoCloudUserData("#!/bin/bash\necho 'hello'\n")),
			)

			By("setting disk caches")
			// ephemeral-disk1
			vmi.Spec.Domain.Devices.Disks[0].Cache = v1.CacheNone
			// ephemeral-disk2
			vmi.Spec.Domain.Devices.Disks[1].Cache = v1.CacheWriteThrough
			// ephemeral-disk5
			vmi.Spec.Domain.Devices.Disks[2].Cache = v1.CacheWriteBack

			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			disks := runningVMISpec.Devices.Disks
			By("checking if number of attached disks is equal to real disks number")
			Expect(vmi.Spec.Domain.Devices.Disks).To(HaveLen(len(disks)))

			cacheNone := string(v1.CacheNone)
			cacheWritethrough := string(v1.CacheWriteThrough)
			cacheWriteback := string(v1.CacheWriteBack)

			By("checking if requested cache 'none' has been set")
			Expect(disks[0].Alias.GetName()).To(Equal("ephemeral-disk1"))
			Expect(disks[0].Driver.Cache).To(Equal(cacheNone))

			By("checking if requested cache 'writethrough' has been set")
			Expect(disks[1].Alias.GetName()).To(Equal("ephemeral-disk2"))
			Expect(disks[1].Driver.Cache).To(Equal(cacheWritethrough))

			By("checking if requested cache 'writeback' has been set")
			Expect(disks[2].Alias.GetName()).To(Equal("ephemeral-disk5"))
			Expect(disks[2].Driver.Cache).To(Equal(cacheWriteback))

			By("checking if default cache 'none' has been set to ephemeral disk")
			Expect(disks[3].Alias.GetName()).To(Equal("ephemeral-disk3"))
			Expect(disks[3].Driver.Cache).To(Equal(cacheNone))

			By("checking if default cache 'none' has been set to cloud-init disk")
			Expect(disks[4].Alias.GetName()).To(Equal(libvmi.CloudInitDiskName))
			Expect(disks[4].Driver.Cache).To(Equal(cacheNone))
		})

		It("[test_id:5360]should set appropriate IO modes", decorators.RequiresBlockStorage, func() {
			By("Creating block Datavolume")
			sc, foundSC := libstorage.GetBlockStorageClass(k8sv1.ReadWriteOnce)
			if !foundSC {
				Fail("Block storage RWO is not present")
			}

			dataVolume := libdv.NewDataVolume(
				libdv.WithRegistryURLSource(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskCirros)),
				libdv.WithStorage(libdv.StorageWithStorageClass(sc), libdv.StorageWithBlockVolumeMode()),
			)
			dataVolume, err := virtClient.CdiClient().CdiV1beta1().DataVolumes(testsuite.GetTestNamespace(nil)).Create(context.Background(), dataVolume, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libstorage.EventuallyDV(dataVolume, 240, Or(HaveSucceeded(), WaitForFirstConsumer()))

			const alpineHostPath = "alpine-host-path"
			libstorage.CreateHostPathPv(alpineHostPath, testsuite.GetTestNamespace(nil), testsuite.HostPathAlpine)
			libstorage.CreateHostPathPVC(alpineHostPath, testsuite.GetTestNamespace(nil), "1Gi")
			vmi := libvmi.New(
				libvmi.WithMemoryRequest("128Mi"),
				// disk[0]
				libvmi.WithContainerDisk("ephemeral-disk1", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
				// disk[1]:  Block, no user-input, cache=none
				libvmi.WithPersistentVolumeClaim("block-pvc", dataVolume.Name),
				// disk[2]: File, not-sparsed, no user-input, cache=none
				libvmi.WithPersistentVolumeClaim("hostpath-pvc", fmt.Sprintf("disk-%s", alpineHostPath)),
				// disk[3]
				libvmi.WithContainerDisk("ephemeral-disk2", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
			)
			// disk[0]:  File, sparsed, no user-input, cache=none
			vmi.Spec.Domain.Devices.Disks[0].Cache = v1.CacheNone
			// disk[3]:  File, sparsed, user-input=threads, cache=none
			vmi.Spec.Domain.Devices.Disks[3].Cache = v1.CacheNone
			vmi.Spec.Domain.Devices.Disks[3].IO = v1.IOThreads

			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			disks := runningVMISpec.Devices.Disks
			By("checking if number of attached disks is equal to real disks number")
			Expect(vmi.Spec.Domain.Devices.Disks).To(HaveLen(len(disks)))

			ioNative := v1.IONative
			ioThreads := v1.IOThreads
			ioNone := ""

			By("checking if default io has not been set for sparsed file")
			Expect(disks[0].Alias.GetName()).To(Equal("ephemeral-disk1"))
			Expect(string(disks[0].Driver.IO)).To(Equal(ioNone))

			By("checking if default io mode has been set to 'native' for block device")
			Expect(disks[1].Alias.GetName()).To(Equal("block-pvc"))
			Expect(disks[1].Driver.IO).To(Equal(ioNative))

			By("checking if default cache 'none' has been set to pvc disk")
			Expect(disks[2].Alias.GetName()).To(Equal("hostpath-pvc"))
			// PVC is mounted as tmpfs on kind, which does not support direct I/O.
			// As such, it behaves as plugging in a hostDisk - check disks[6].
			if checks.IsRunningOnKindInfra() {
				// The cache mode is set to cacheWritethrough
				Expect(string(disks[2].Driver.IO)).To(Equal(ioNone))
			} else {
				// The cache mode is set to cacheNone
				Expect(disks[2].Driver.IO).To(Equal(ioNative))
			}

			By("checking if requested io mode 'threads' has been set")
			Expect(disks[3].Alias.GetName()).To(Equal("ephemeral-disk2"))
			Expect(disks[3].Driver.IO).To(Equal(ioThreads))

		})
	})

	Context("Block size configuration set", func() {

		It("[test_id:6965]Should set BlockIO when using custom block sizes", decorators.SigStorage, decorators.RequiresBlockStorage, func() {

			By("creating a block volume")
			sc, foundSC := libstorage.GetBlockStorageClass(k8sv1.ReadWriteOnce)
			if !foundSC {
				Fail(`Block storage is not present. You can filter by "RequiresBlockStorage" label`)
			}

			dataVolume := libdv.NewDataVolume(
				libdv.WithRegistryURLSource(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskCirros)),
				libdv.WithStorage(libdv.StorageWithStorageClass(sc), libdv.StorageWithBlockVolumeMode()),
			)
			dataVolume, err := virtClient.CdiClient().CdiV1beta1().DataVolumes(testsuite.GetTestNamespace(nil)).Create(context.Background(), dataVolume, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libstorage.EventuallyDV(dataVolume, 240, Or(HaveSucceeded(), WaitForFirstConsumer()))

			vmi := libvmi.New(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
				libvmi.WithPersistentVolumeClaim("disk0", dataVolume.Name),
				libvmi.WithMemoryRequest("128Mi"),
			)

			By("setting the disk to use custom block sizes")
			logicalSize := uint(16384)
			physicalSize := uint(16384)
			vmi.Spec.Domain.Devices.Disks[0].BlockSize = &v1.BlockSize{
				Custom: &v1.CustomBlockSize{
					Logical:  logicalSize,
					Physical: physicalSize,
				},
			}

			By("initializing the VM")
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			By("checking if number of attached disks is equal to real disks number")
			disks := runningVMISpec.Devices.Disks
			Expect(vmi.Spec.Domain.Devices.Disks).To(HaveLen(len(disks)))

			By("checking if BlockIO is set to the custom block size")
			Expect(disks[0].Alias.GetName()).To(Equal("disk0"))
			Expect(disks[0].BlockIO).ToNot(BeNil())
			Expect(disks[0].BlockIO.LogicalBlockSize).To(Equal(logicalSize))
			Expect(disks[0].BlockIO.PhysicalBlockSize).To(Equal(physicalSize))
		})

		It("[test_id:6966]Should set BlockIO when set to match volume block sizes on block devices", decorators.SigStorage, decorators.RequiresBlockStorage, func() {

			By("creating a block volume")
			sc, foundSC := libstorage.GetBlockStorageClass(k8sv1.ReadWriteOnce)
			if !foundSC {
				Fail(`Block storage is not present. You can skip by "RequiresBlockStorage" label`)
			}

			dataVolume := libdv.NewDataVolume(
				libdv.WithRegistryURLSource(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskCirros)),
				libdv.WithStorage(libdv.StorageWithStorageClass(sc), libdv.StorageWithBlockVolumeMode()),
			)
			dataVolume, err := virtClient.CdiClient().CdiV1beta1().DataVolumes(testsuite.GetTestNamespace(nil)).Create(context.Background(), dataVolume, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libstorage.EventuallyDV(dataVolume, 240, Or(HaveSucceeded(), WaitForFirstConsumer()))

			vmi := libvmi.New(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
				libvmi.WithPersistentVolumeClaim("disk0", dataVolume.Name),
				libvmi.WithMemoryRequest("128Mi"),
			)

			By("setting the disk to match the volume block sizes")
			vmi.Spec.Domain.Devices.Disks[0].BlockSize = &v1.BlockSize{
				MatchVolume: &v1.FeatureState{},
			}

			By("initializing the VM")
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			By("checking if number of attached disks is equal to real disks number")
			disks := runningVMISpec.Devices.Disks
			Expect(vmi.Spec.Domain.Devices.Disks).To(HaveLen(len(disks)))

			By("checking if BlockIO is set for the disk")
			Expect(disks[0].Alias.GetName()).To(Equal("disk0"))
			Expect(disks[0].BlockIO).ToNot(BeNil())
			// Block devices should be one of 512n, 512e or 4096n so accept 512 and 4096 values.
			expectedDiskSizes := SatisfyAny(Equal(uint(512)), Equal(uint(4096)))
			Expect(disks[0].BlockIO.LogicalBlockSize).To(expectedDiskSizes)
			Expect(disks[0].BlockIO.PhysicalBlockSize).To(expectedDiskSizes)
		})

		It("[test_id:6967]Should set BlockIO when set to match volume block sizes on files", decorators.HostDiskGate, func() {
			if !checks.HasFeature(featuregate.HostDiskGate) {
				Fail("Cluster has the HostDisk featuregate disabled, use skip for HostDiskGate")
			}

			By("creating a disk image")
			var nodeName string
			tmpHostDiskDir := storage.RandHostDiskDir()
			tmpHostDiskPath := filepath.Join(tmpHostDiskDir, fmt.Sprintf("disk-%s.img", uuid.NewString()))

			pod := storage.CreateHostDisk(tmpHostDiskPath)
			pod, err := virtClient.CoreV1().Pods(testsuite.NamespacePrivileged).Create(context.Background(), pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Eventually(ThisPod(pod), 30*time.Second, 1*time.Second).Should(BeInPhase(k8sv1.PodSucceeded))
			pod, err = ThisPod(pod)()
			Expect(err).NotTo(HaveOccurred())
			nodeName = pod.Spec.NodeName
			defer func() {
				Expect(storage.RemoveHostDisk(tmpHostDiskDir, nodeName)).To(Succeed())
			}()

			vmi := libvmi.New(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
				libvmi.WithMemoryRequest("128Mi"),
				libvmi.WithHostDisk("host-disk", tmpHostDiskPath, v1.HostDiskExists),
				libvmi.WithNodeAffinityFor(nodeName),
				// hostdisk needs a privileged namespace
				libvmi.WithNamespace(testsuite.NamespacePrivileged),
			)

			By("setting the disk to match the volume block sizes")
			vmi.Spec.Domain.Devices.Disks[0].BlockSize = &v1.BlockSize{
				MatchVolume: &v1.FeatureState{},
			}

			By("initializing the VM")
			vmi = libvmops.RunVMIAndExpectLaunch(vmi, 60)
			runningVMISpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			By("checking if number of attached disks is equal to real disks number")
			disks := runningVMISpec.Devices.Disks
			Expect(vmi.Spec.Domain.Devices.Disks).To(HaveLen(len(disks)))

			By("checking if BlockIO is set for the disk")
			Expect(disks[0].Alias.GetName()).To(Equal("host-disk"))
			Expect(disks[0].BlockIO).ToNot(BeNil())
			// The default for most filesystems nowadays is 4096 but it can be changed.
			// As such, relying on a specific value is flakey.
			// As long as we have a value, the exact value doesn't matter.
			Expect(disks[0].BlockIO.LogicalBlockSize).ToNot(BeZero())
			// A filesystem only has a single size so logical == physical
			Expect(disks[0].BlockIO.LogicalBlockSize).To(Equal(disks[0].BlockIO.PhysicalBlockSize))
		})
	})

	Context("[rfe_id:898][crit:medium][vendor:cnv-qe@redhat.com][level:component]New VirtualMachineInstance with all supported drives", func() {

		// ordering:
		// use a small disk for the other ones
		testVMI := func() *v1.VirtualMachineInstance {
			// virtio - added by NewCirros
			return libvmifact.NewCirros(
				// add sata disk
				libvmi.WithContainerSATADisk("disk2", cd.ContainerDiskFor(cd.ContainerDiskCirros)),
			)
			// NOTE: we have one disk per bus, so we expect vda, sda
		}

		checkPciAddress := func(vmi *v1.VirtualMachineInstance, expectedPciAddress string) {
			err := console.SafeExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "grep DEVNAME /sys/bus/pci/devices/" + expectedPciAddress + "/*/block/vda/uevent|awk -F= '{ print $2 }'\n"},
				&expect.BExp{R: "vda"},
			}, 15)
			Expect(err).ToNot(HaveOccurred())
		}

		It("[test_id:1682]should have all the device nodes", func() {
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), testVMI(), metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitForSuccessfulVMIStart(vmi)

			Expect(console.LoginToCirros(vmi)).To(Succeed())

			Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
				// keep the ordering!
				&expect.BSnd{S: "ls /dev/vda  /dev/vdb\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "echo $?\n"},
				&expect.BExp{R: console.RetValue("0")},
			}, 10)).To(Succeed())
		})

		It("[test_id:3906]should configure custom Pci address", func() {
			By("checking disk1 Pci address")
			vmi := testVMI()
			vmi.Spec.Domain.Devices.Disks[0].Disk.PciAddress = "0000:00:10.0"
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = v1.DiskBusVirtio
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitUntilVMIReady(vmi, console.LoginToCirros)

			checkPciAddress(vmi, vmi.Spec.Domain.Devices.Disks[0].Disk.PciAddress)
		})

		It("[test_id:1020]should not create the VM with wrong PCI address", func() {
			By("setting disk1 Pci address")

			wrongPciAddress := "0000:04:10.0"

			vmi := testVMI()
			vmi.Spec.Domain.Devices.Disks[0].Disk.PciAddress = wrongPciAddress
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = v1.DiskBusVirtio
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			var vmiCondition v1.VirtualMachineInstanceCondition
			Eventually(func() bool {
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Get(context.Background(), vmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())

				for _, cond := range vmi.Status.Conditions {
					if cond.Type == v1.VirtualMachineInstanceConditionType(v1.VirtualMachineInstanceSynchronized) && cond.Status == k8sv1.ConditionFalse {
						vmiCondition = cond
						return true
					}
				}
				return false
			}, 120*time.Second, time.Second).Should(BeTrue())

			Expect(vmiCondition.Message).To(ContainSubstring("Invalid PCI address " + wrongPciAddress))
			Expect(vmiCondition.Reason).To(Equal("Synchronizing with the Domain failed."))
		})
	})
	Describe("[rfe_id:897][crit:medium][vendor:cnv-qe@redhat.com][level:component]VirtualMachineInstance with CPU pinning", decorators.WgArm64, decorators.RequiresTwoWorkerNodesWithCPUManager, func() {
		isNodeHasCPUManagerLabel := func(nodeName string) bool {

			nodeObject, err := virtClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			nodeHaveCpuManagerLabel := false
			nodeLabels := nodeObject.GetLabels()

			for label, val := range nodeLabels {
				if label == v1.CPUManager && val == "true" {
					nodeHaveCpuManagerLabel = true
					break
				}
			}
			return nodeHaveCpuManagerLabel
		}

		BeforeEach(func() {
			nodes, err := virtClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			if len(nodes.Items) == 1 {
				Fail(`CPU pinning test that requires multiple nodes when only one node is present. You can filter by "requires-two-worker-nodes-with-cpu-manager" label`)
			}
		})

		Context("with cpu pinning enabled", Serial, func() {
			It("[test_id:1685]non master node should have a cpumanager label", func() {
				cpuManagerEnabled := false
				nodes, err := virtClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				for idx := 1; idx < len(nodes.Items); idx++ {
					labels := nodes.Items[idx].GetLabels()
					for label, val := range labels {
						if label == "cpumanager" && val == "true" {
							cpuManagerEnabled = true
						}
					}
				}
				Expect(cpuManagerEnabled).To(BeTrue())
			})
			It("[test_id:991]should be scheduled on a node with running cpu manager", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					Cores:                 2,
					DedicatedCPUPlacement: true,
				}

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node := libwait.WaitForSuccessfulVMIStart(cpuVmi).Status.NodeName

				By("Checking that the VMI QOS is guaranteed")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Get(context.Background(), cpuVmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Status.QOSClass).ToNot(BeNil())
				Expect(*vmi.Status.QOSClass).To(Equal(k8sv1.PodQOSGuaranteed))

				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())

				By("Checking that the pod QOS is guaranteed")
				readyPod, err := libpod.GetPodByVirtualMachineInstance(cpuVmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())
				podQos := readyPod.Status.QOSClass
				Expect(podQos).To(Equal(k8sv1.PodQOSGuaranteed))

				var computeContainer *k8sv1.Container
				for _, container := range readyPod.Spec.Containers {
					if container.Name == "compute" {
						computeContainer = &container
					}
				}
				Expect(computeContainer).ToNot(BeNil(), "could not find the computer container")

				output, err := getPodCPUSet(readyPod)
				log.Log.Infof("%v", output)
				Expect(err).ToNot(HaveOccurred())
				output = strings.TrimSuffix(output, "\n")
				pinnedCPUsList, err := hw_utils.ParseCPUSetLine(output, 100)
				Expect(err).ToNot(HaveOccurred())

				Expect(pinnedCPUsList).To(HaveLen(int(cpuVmi.Spec.Domain.CPU.Cores)))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the number of CPU cores under guest OS")
				Expect(console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^processor /proc/cpuinfo\n"},
					&expect.BExp{R: "2"},
				}, 15)).To(Succeed())

				By("Check values in domain XML")
				domXML, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, cpuVmi)
				Expect(err).ToNot(HaveOccurred(), "Should return XML from VMI")
				Expect(domXML).To(ContainSubstring("<hint-dedicated state='on'/>"), "should container the hint-dedicated feature")
			})
			It("[test_id:4632]should be able to start a vm with guest memory different from requested and keep guaranteed qos", func() {
				Skip("Skip test till issue https://github.com/kubevirt/kubevirt/issues/3910 is fixed")
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					Sockets:               2,
					Cores:                 1,
					DedicatedCPUPlacement: true,
				}
				guestMemory := resource.MustParse("64M")
				cpuVmi.Spec.Domain.Memory = &v1.Memory{Guest: &guestMemory}
				cpuVmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("80M"),
					},
				}

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node := libwait.WaitForSuccessfulVMIStart(cpuVmi).Status.NodeName

				By("Checking that the VMI QOS is guaranteed")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Get(context.Background(), cpuVmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Status.QOSClass).ToNot(BeNil())
				Expect(*vmi.Status.QOSClass).To(Equal(k8sv1.PodQOSGuaranteed))

				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())

				By("Checking that the pod QOS is guaranteed")
				readyPod, err := libpod.GetPodByVirtualMachineInstance(cpuVmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())
				podQos := readyPod.Status.QOSClass
				Expect(podQos).To(Equal(k8sv1.PodQOSGuaranteed))

				// -------------------------------------------------------------------
				Expect(console.LoginToCirros(vmi)).To(Succeed())

				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "[ $(free -m | grep Mem: | tr -s ' ' | cut -d' ' -f2) -lt 80 ] && echo 'pass'\n"},
					&expect.BExp{R: console.RetValue("pass")},
					&expect.BSnd{S: "swapoff -a && dd if=/dev/zero of=/dev/shm/test bs=1k count=118k\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: "echo $?\n"},
					&expect.BExp{R: console.RetValue("0")},
				}, 15)).To(Succeed())

				pod, err := libpod.GetPodByVirtualMachineInstance(vmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())

				podMemoryUsage, err := getPodMemoryUsage(pod)
				Expect(err).ToNot(HaveOccurred())
				By("Converting pod memory usage")
				m, err := strconv.Atoi(strings.Trim(podMemoryUsage, "\n"))
				Expect(err).ToNot(HaveOccurred())
				By("Checking if pod memory usage is > 80Mi")
				Expect(m).To(BeNumerically(">", 83886080), "83886080 B = 80 Mi")
			})
			DescribeTable("[test_id:4023]should start a vmi with dedicated cpus and isolated emulator thread", func(resources *v1.ResourceRequirements) {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					Cores:                 2,
					DedicatedCPUPlacement: true,
					IsolateEmulatorThread: true,
				}
				if resources != nil {
					cpuVmi.Spec.Domain.Resources = *resources
				}

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node := libwait.WaitForSuccessfulVMIStart(cpuVmi).Status.NodeName

				By("Checking that the VMI QOS is guaranteed")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Get(context.Background(), cpuVmi.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Status.QOSClass).ToNot(BeNil())
				Expect(*vmi.Status.QOSClass).To(Equal(k8sv1.PodQOSGuaranteed))

				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())

				By("Checking that the pod QOS is guaranteed")
				readyPod, err := libpod.GetPodByVirtualMachineInstance(cpuVmi, vmi.Namespace)
				Expect(err).NotTo(HaveOccurred())

				podQos := readyPod.Status.QOSClass
				Expect(podQos).To(Equal(k8sv1.PodQOSGuaranteed))

				var computeContainer *k8sv1.Container
				for _, container := range readyPod.Spec.Containers {
					if container.Name == "compute" {
						computeContainer = &container
					}
				}
				Expect(computeContainer).ToNot(BeNil(), "could not find the compute container")

				output, err := getPodCPUSet(readyPod)
				log.Log.Infof("%v", output)
				Expect(err).ToNot(HaveOccurred())
				output = strings.TrimSuffix(output, "\n")
				pinnedCPUsList, err := hw_utils.ParseCPUSetLine(output, 100)
				Expect(err).ToNot(HaveOccurred())

				output, err = listCgroupThreads(readyPod)
				Expect(err).ToNot(HaveOccurred())
				pids := strings.Split(output, "\n")

				getProcessNameErrors := 0
				By("Expecting only vcpu threads on root of pod cgroup")
				for _, pid := range pids {
					if len(pid) == 0 {
						continue
					}
					output, err = getProcessName(readyPod, pid)
					if err != nil {
						getProcessNameErrors++
						continue
					}
					Expect(output).To(ContainSubstring("CPU "))
					Expect(output).To(ContainSubstring("KVM"))
				}
				Expect(getProcessNameErrors).Should(BeNumerically("<=", 1))

				// 1 additioan pcpus should be allocated on the pod for the emulation threads
				Expect(pinnedCPUsList).To(HaveLen(int(cpuVmi.Spec.Domain.CPU.Cores) + 1))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the number of CPU cores under guest OS")
				Expect(console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^processor /proc/cpuinfo\n"},
					&expect.BExp{R: "2"},
				}, 15)).To(Succeed())

				domSpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
				Expect(err).ToNot(HaveOccurred())

				emulator := filepath.Base(domSpec.Devices.Emulator)
				pidCmd := []string{"pidof", emulator}
				qemuPid, err := exec.ExecuteCommandOnPod(readyPod, "compute", pidCmd)
				// do not check for kvm-pit thread if qemu is not in use
				if err != nil {
					return
				}
				kvmpitmask, err := getKvmPitMask(strings.TrimSpace(qemuPid), node)
				Expect(err).ToNot(HaveOccurred())

				vcpuzeromask, err := getVcpuMask(readyPod, emulator, "0")
				Expect(err).ToNot(HaveOccurred())

				Expect(kvmpitmask).To(Equal(vcpuzeromask))
			},
				Entry(" with explicit resources set", &v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceCPU:    resource.MustParse("2"),
						k8sv1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: k8sv1.ResourceList{
						k8sv1.ResourceCPU:    resource.MustParse("2"),
						k8sv1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}),
				Entry("without resource requirements set", nil),
			)

			It("[test_id:4024]should fail the vmi creation if IsolateEmulatorThread requested without dedicated cpus", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					Cores:                 2,
					IsolateEmulatorThread: true,
				}

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
			})

			It("[test_id:802]should configure correct number of vcpus with requests.cpus", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					DedicatedCPUPlacement: true,
				}
				cpuVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("2")

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node := libwait.WaitForSuccessfulVMIStart(cpuVmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToCirros(cpuVmi)).To(Succeed())

				By("Checking the number of CPU cores under guest OS")
				Expect(console.SafeExpectBatch(cpuVmi, []expect.Batcher{
					&expect.BSnd{S: "grep -c ^processor /proc/cpuinfo\n"},
					&expect.BExp{R: "2"},
				}, 15)).To(Succeed())
			})

			It("[test_id:1688]should fail the vmi creation if the requested resources are inconsistent", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					Cores:                 2,
					DedicatedCPUPlacement: true,
				}

				cpuVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("3")

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
			})
			It("[test_id:1689]should fail the vmi creation if cpu is not an integer", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					DedicatedCPUPlacement: true,
				}

				cpuVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("300m")

				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
			})
			It("[test_id:1690]should fail the vmi creation if Guaranteed QOS cannot be set", func() {
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					DedicatedCPUPlacement: true,
				}
				cpuVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("2")
				cpuVmi.Spec.Domain.Resources = v1.ResourceRequirements{
					Limits: k8sv1.ResourceList{
						k8sv1.ResourceCPU: resource.MustParse("4"),
					},
				}
				By("Starting a VirtualMachineInstance")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).To(HaveOccurred())
			})
			It("[test_id:830]should start a vm with no cpu pinning after a vm with cpu pinning on same node", func() {
				Vmi := libvmifact.NewCirros()
				cpuVmi := libvmifact.NewCirros()
				cpuVmi.Spec.Domain.CPU = &v1.CPU{
					DedicatedCPUPlacement: true,
				}

				cpuVmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("2")
				Vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("1")
				Vmi.Spec.NodeSelector = map[string]string{v1.CPUManager: "true"}

				By("Starting a VirtualMachineInstance with dedicated cpus")
				cpuVmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), cpuVmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node := libwait.WaitForSuccessfulVMIStart(cpuVmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())

				By("Starting a VirtualMachineInstance without dedicated cpus")
				Vmi, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuVmi)).Create(context.Background(), Vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node = libwait.WaitForSuccessfulVMIStart(Vmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node)).To(BeTrue())
			})
		})

		Context("cpu pinning with fedora images, dedicated and non dedicated cpu should be possible on same node via spec.domain.cpu.cores", Serial, func() {
			var node string

			dedicatedCPUVMI := func() *v1.VirtualMachineInstance {
				return libvmifact.NewFedora(
					libvmi.WithCPUCount(2, 0, 0),
					libvmi.WithDedicatedCPUPlacement(),
					libvmi.WithMemoryRequest("512M"),
					libvmi.WithNodeSelectorFor(node),
				)
			}
			noDedicatedCPUVMI := func() *v1.VirtualMachineInstance {
				return libvmifact.NewFedora(
					libvmi.WithCPUCount(2, 0, 0),
					libvmi.WithDedicatedCPUPlacement(),
					libvmi.WithMemoryRequest("512M"),
					libvmi.WithNodeSelectorFor(node),
				)
			}

			BeforeEach(func() {
				nodes := libnode.GetAllSchedulableNodes(virtClient)
				Expect(nodes.Items).ToNot(BeEmpty(), "There should be some nodes")
				node = nodes.Items[1].Name
			})

			It("[test_id:829]should start a vm with no cpu pinning after a vm with cpu pinning on same node", func() {
				cpuvmi := dedicatedCPUVMI()
				By("Starting a VirtualMachineInstance with dedicated cpus")
				cpuvmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuvmi)).Create(context.Background(), cpuvmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node1 := libwait.WaitForSuccessfulVMIStart(cpuvmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node1)).To(BeTrue())
				Expect(node1).To(Equal(node))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(cpuvmi)).To(Succeed())

				By("Starting a VirtualMachineInstance without dedicated cpus")
				vmi := noDedicatedCPUVMI()
				vmi, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node2 := libwait.WaitForSuccessfulVMIStart(vmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node2)).To(BeTrue())
				Expect(node2).To(Equal(node))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(vmi)).To(Succeed())
			})

			It("[test_id:832]should start a vm with cpu pinning after a vm with no cpu pinning on same node", func() {
				vmi := noDedicatedCPUVMI()
				By("Starting a VirtualMachineInstance without dedicated cpus")
				vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node2 := libwait.WaitForSuccessfulVMIStart(vmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node2)).To(BeTrue())
				Expect(node2).To(Equal(node))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(vmi)).To(Succeed())

				By("Starting a VirtualMachineInstance with dedicated cpus")
				cpuvmi := dedicatedCPUVMI()
				cpuvmi, err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(cpuvmi)).Create(context.Background(), cpuvmi, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				node1 := libwait.WaitForSuccessfulVMIStart(cpuvmi).Status.NodeName
				Expect(isNodeHasCPUManagerLabel(node1)).To(BeTrue())
				Expect(node1).To(Equal(node))

				By("Expecting the VirtualMachineInstance console")
				Expect(console.LoginToFedora(cpuvmi)).To(Succeed())
			})
		})
	})

	Context("[rfe_id:2926][crit:medium][vendor:cnv-qe@redhat.com][level:component]Check Chassis value", func() {

		It("[test_id:2927]Test Chassis value in a newly created VM", Serial, func() {
			vmi := libvmifact.NewFedora()
			vmi.Spec.Domain.Chassis = &v1.Chassis{
				Asset: "Test-123",
			}

			By("Starting a VirtualMachineInstance")
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitForSuccessfulVMIStart(vmi)

			By("Check values on domain XML")
			domXml, err := libdomain.GetRunningVirtualMachineInstanceDomainXML(virtClient, vmi)
			Expect(err).ToNot(HaveOccurred())
			Expect(domXml).To(ContainSubstring("<entry name='asset'>Test-123</entry>"))

			By("Expecting console")
			Expect(console.LoginToFedora(vmi)).To(Succeed())

			By("Check value in VM with dmidecode")
			// Check on the VM, if expected values are there with dmidecode
			Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: "[ $(sudo dmidecode -s chassis-asset-tag | tr -s ' ') = Test-123 ] && echo 'pass'\n"},
				&expect.BExp{R: console.RetValue("pass")},
			}, 10)).To(Succeed())
		})
	})

	Context("[rfe_id:2926][crit:medium][vendor:cnv-qe@redhat.com][level:component]Check SMBios with default and custom values", func() {

		It("[test_id:2751]test default SMBios", func() {
			kv := libkubevirt.GetCurrentKv(virtClient)
			config := kv.Spec.Configuration
			smBIOS := config.SMBIOSConfig
			if smBIOS == nil {
				smBIOS = &v1.SMBiosConfiguration{
					Manufacturer: "KubeVirt",
					Product:      "None",
					Family:       "KubeVirt",
				}
			}

			By("Starting a VirtualMachineInstance")
			vmi := libvmifact.NewFedora()
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitForSuccessfulVMIStart(vmi)

			By("Expecting console")
			Expect(console.LoginToFedora(vmi)).To(Succeed())

			By("Check values in dmidecode")
			// Check on the VM, if expected values are there with dmidecode
			Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: fmt.Sprintf(`[ "$(sudo dmidecode -s system-family | tr -s ' ')" = "%s" ] && echo 'pass'`+"\n", smBIOS.Family)},
				&expect.BExp{R: console.RetValue("pass")},
				&expect.BSnd{S: fmt.Sprintf(`[ "$(sudo dmidecode -s system-product-name | tr -s ' ')" = "%s" ] && echo 'pass'`+"\n", smBIOS.Product)},
				&expect.BExp{R: console.RetValue("pass")},
				&expect.BSnd{S: fmt.Sprintf(`[ "$(sudo dmidecode -s system-manufacturer | tr -s ' ')" = "%s" ] && echo 'pass'`+"\n", smBIOS.Manufacturer)},
				&expect.BExp{R: console.RetValue("pass")},
			}, 1)).To(Succeed())
		})

	})

	Context("Custom PCI Addresses configuration", func() {
		// The aim of the test is to validate the configurability of a range of PCI slots
		// on the root PCI bus 0. We would like to test slots 2..1a (slots 0,1 and beyond 1a are reserved).
		// In addition , we test usage of PCI functions on a single slot
		// by occupying all the functions 1..7 on random port 2.

		addrPrefix := "0000:00" // PCI bus 0
		numOfSlotsToTest := 24  // slots 2..1a
		numOfFuncsToTest := 8

		createDisks := func(numOfDisks int, vmi *v1.VirtualMachineInstance) {
			for i := 0; i < numOfDisks; i++ {
				vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
					v1.Disk{
						Name: fmt.Sprintf("test%v", i),
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: v1.DiskBusVirtio,
							},
						},
					})
				vmi.Spec.Volumes = append(vmi.Spec.Volumes,
					v1.Volume{
						Name: fmt.Sprintf("test%v", i),
						VolumeSource: v1.VolumeSource{
							EmptyDisk: &v1.EmptyDiskSource{
								Capacity: resource.MustParse("1Mi"),
							},
						},
					})
			}
		}
		assignDisksToSlots := func(startIndex int, vmi *v1.VirtualMachineInstance) {
			var addr string

			for i, disk := range vmi.Spec.Domain.Devices.Disks {
				addr = fmt.Sprintf("%x", i+startIndex)
				if len(addr) == 1 {
					disk.DiskDevice.Disk.PciAddress = fmt.Sprintf("%s:0%v.0", addrPrefix, addr)
				} else {
					disk.DiskDevice.Disk.PciAddress = fmt.Sprintf("%s:%v.0", addrPrefix, addr)
				}
			}
		}

		assignDisksToFunctions := func(startIndex int, vmi *v1.VirtualMachineInstance) {
			for i, disk := range vmi.Spec.Domain.Devices.Disks {
				disk.DiskDevice.Disk.PciAddress = fmt.Sprintf("%s:02.%v", addrPrefix, fmt.Sprintf("%x", i+startIndex))
			}
		}

		DescribeTable("should configure custom pci address", func(startIndex, numOfDevices int, testingPciFunctions bool) {
			const bootOrder uint = 1
			vmi := libvmifact.NewFedora(
				libnet.WithMasqueradeNetworking(),
				libvmi.WithMemoryRequest("1024M"),
			)
			vmi.Spec.Domain.Devices.Disks[0].BootOrder = pointer.P(bootOrder)

			currentDisks := len(vmi.Spec.Domain.Devices.Disks)
			numOfDisksToAdd := numOfDevices - currentDisks

			createDisks(numOfDisksToAdd, vmi)
			if testingPciFunctions {
				assignDisksToFunctions(startIndex, vmi)
			} else {
				kvconfig.DisableFeatureGate(featuregate.ExpandDisksGate)
				assignDisksToSlots(startIndex, vmi)
			}
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitUntilVMIReady(vmi, console.LoginToFedora)
			Expect(vmi.Spec.Domain.Devices.Disks).Should(HaveLen(numOfDevices))

			err = virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Delete(context.Background(), vmi.Name, metav1.DeleteOptions{})
			Expect(err).ToNot(HaveOccurred())
		},
			Entry("[test_id:5269]across all available PCI root bus slots", Serial, 2, numOfSlotsToTest, false),
			Entry("[test_id:5270]across all available PCI functions of a single slot", 0, numOfFuncsToTest, true),
		)
	})

	Context("Check KVM CPUID advertisement", func() {

		It("[test_id:5271]test cpuid hidden", func() {
			vmi := libvmifact.NewFedora()
			vmi.Spec.Domain.Features = &v1.Features{
				KVM: &v1.FeatureKVM{Hidden: true},
			}

			By("Starting a VirtualMachineInstance")
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitForSuccessfulVMIStart(vmi)

			By("Expecting console")
			Expect(console.LoginToFedora(vmi)).To(Succeed())

			By("Check virt-what-cpuid-helper does not match KVM")
			Expect(console.ExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: "/usr/libexec/virt-what-cpuid-helper > /dev/null 2>&1 && echo 'pass'\n"},
				&expect.BExp{R: console.RetValue("pass")},
				&expect.BSnd{S: "$(sudo /usr/libexec/virt-what-cpuid-helper | grep -q KVMKVMKVM) || echo 'pass'\n"},
				&expect.BExp{R: console.RetValue("pass")},
			}, 2*time.Second)).To(Succeed())
		})

	})
	Context("virt-launcher processes memory usage", func() {
		doesntExceedMemoryUsage := func(processRss *map[string]resource.Quantity, process string, memoryLimit resource.Quantity) {
			actual := (*processRss)[process]
			ExpectWithOffset(1, (&actual).Cmp(memoryLimit)).To(Equal(-1),
				"the %s process is taking too much RAM! (%s > %s). All processes: %v",
				process, actual.String(), memoryLimit.String(), processRss)
		}
		It("should be lower than allocated size", func() {
			By("Starting a VirtualMachineInstance")
			vmi := libvmifact.NewFedora(libnet.WithMasqueradeNetworking())
			vmi, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(vmi)).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			libwait.WaitForSuccessfulVMIStart(vmi)

			By("Expecting console")
			Expect(console.LoginToFedora(vmi)).To(Succeed())

			By("Running ps in virt-launcher")
			pods, err := virtClient.CoreV1().Pods(vmi.Namespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: v1.CreatedByLabel + "=" + string(vmi.GetUID()),
			})
			Expect(err).ToNot(HaveOccurred(), "Should list pods successfully")
			var stdout, stderr string
			errorMassageFormat := "failed after running the `ps` command with stdout:\n %v \n stderr:\n %v \n err: \n %v \n"
			Eventually(func() error {
				stdout, stderr, err = exec.ExecuteCommandOnPodWithResults(&pods.Items[0], "compute",
					[]string{
						"ps",
						"--no-header",
						"axo",
						"rss,command",
					})
				return err
			}, time.Second, 50*time.Millisecond).Should(Succeed(), fmt.Sprintf(errorMassageFormat, stdout, stderr, err))

			By("Parsing the output of ps")
			processRss := make(map[string]resource.Quantity)
			scanner := bufio.NewScanner(strings.NewReader(stdout))
			for scanner.Scan() {
				fields := strings.Fields(scanner.Text())
				Expect(len(fields)).To(BeNumerically(">=", 2))
				rss := fields[0]
				command := filepath.Base(fields[1])
				// Handle the qemu binary: e.g. qemu-kvm or qemu-system-x86_64
				if command == "qemu-kvm" || strings.HasPrefix(command, "qemu-system-") {
					command = "qemu"
				}
				switch command {
				case "virt-launcher-monitor", "virt-launcher", "virtlogd", "virtqemud", "qemu":
					Expect(processRss).ToNot(HaveKey(command), "multiple %s processes found", command)
					value := resource.MustParse(rss + "Ki")
					processRss[command] = value
				}
			}
			for _, process := range []string{"virt-launcher-monitor", "virt-launcher", "virtlogd", "virtqemud", "qemu"} {
				Expect(processRss).To(HaveKey(process), "no %s process found", process)
			}

			By("Ensuring no process is using too much ram")
			doesntExceedMemoryUsage(&processRss, "virt-launcher-monitor", resource.MustParse(services.VirtLauncherMonitorOverhead))
			doesntExceedMemoryUsage(&processRss, "virt-launcher", resource.MustParse(services.VirtLauncherOverhead))
			doesntExceedMemoryUsage(&processRss, "virtlogd", resource.MustParse(services.VirtlogdOverhead))
			doesntExceedMemoryUsage(&processRss, "virtqemud", resource.MustParse(services.VirtqemudOverhead))
			qemuExpected := resource.MustParse(services.QemuOverhead)
			qemuExpected.Add(vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceMemory])
			doesntExceedMemoryUsage(&processRss, "qemu", qemuExpected)
		})
	})

})

func createRuntimeClass(name, handler string) error {
	virtCli := kubevirt.Client()

	_, err := virtCli.NodeV1().RuntimeClasses().Create(
		context.Background(),
		&nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Handler:    handler,
		},
		metav1.CreateOptions{},
	)
	return err
}

func deleteRuntimeClass(name string) error {
	virtCli := kubevirt.Client()

	return virtCli.NodeV1().RuntimeClasses().Delete(context.Background(), name, metav1.DeleteOptions{})
}

func withNoRng() libvmi.Option {
	return func(vmi *v1.VirtualMachineInstance) {
		vmi.Spec.Domain.Devices.Rng = nil
	}
}

func overcommitGuestOverhead() libvmi.Option {
	return func(vmi *v1.VirtualMachineInstance) {
		vmi.Spec.Domain.Resources.OvercommitGuestOverhead = true
	}
}

func withMachineType(machineType string) libvmi.Option {
	return func(vmi *v1.VirtualMachineInstance) {
		vmi.Spec.Domain.Machine = &v1.Machine{Type: machineType}
	}
}

func WithSchedulerName(schedulerName string) libvmi.Option {
	return func(vmi *v1.VirtualMachineInstance) {
		vmi.Spec.SchedulerName = schedulerName
	}
}

func withSerialBIOS() libvmi.Option {
	return func(vmi *v1.VirtualMachineInstance) {
		if vmi.Spec.Domain.Firmware == nil {
			vmi.Spec.Domain.Firmware = &v1.Firmware{}
		}
		if vmi.Spec.Domain.Firmware.Bootloader == nil {
			vmi.Spec.Domain.Firmware.Bootloader = &v1.Bootloader{}
		}
		if vmi.Spec.Domain.Firmware.Bootloader.BIOS == nil {
			vmi.Spec.Domain.Firmware.Bootloader.BIOS = &v1.BIOS{}
		}
		vmi.Spec.Domain.Firmware.Bootloader.BIOS.UseSerial = pointer.P(true)
	}
}

func getKvmPitMask(qemupid, nodeName string) (output string, err error) {
	kvmpitcomm := "kvm-pit/" + qemupid
	args := []string{"pgrep", "-f", kvmpitcomm}
	output, err = libnode.ExecuteCommandInVirtHandlerPod(nodeName, args)
	Expect(err).ToNot(HaveOccurred())

	kvmpitpid := strings.TrimSpace(output)
	tasksetcmd := "taskset -c -p " + kvmpitpid + " | cut -f2 -d:"
	args = []string{"/bin/bash", "-c", tasksetcmd}
	output, err = libnode.ExecuteCommandInVirtHandlerPod(nodeName, args)
	Expect(err).ToNot(HaveOccurred())

	return strings.TrimSpace(output), err
}

func listCgroupThreads(pod *k8sv1.Pod) (output string, err error) {
	output, err = exec.ExecuteCommandOnPod(
		pod,
		"compute",
		[]string{"cat", "/sys/fs/cgroup/cpuset/tasks"},
	)

	if err == nil {
		// Cgroup V1
		return
	}
	output, err = exec.ExecuteCommandOnPod(
		pod,
		"compute",
		[]string{"cat", "/sys/fs/cgroup/cgroup.threads"},
	)
	return
}

func getProcessName(pod *k8sv1.Pod, pid string) (output string, err error) {
	fPath := "/proc/" + pid + "/comm"
	output, err = exec.ExecuteCommandOnPod(
		pod,
		"compute",
		[]string{"cat", fPath},
	)

	return
}

func getVcpuMask(pod *k8sv1.Pod, emulator, cpu string) (output string, err error) {
	pscmd := `ps -LC ` + emulator + ` -o lwp,comm | grep "CPU ` + cpu + `"  | cut -f1 -dC`
	args := []string{"/bin/bash", "-c", pscmd}
	Eventually(func() error {
		output, err = exec.ExecuteCommandOnPod(pod, "compute", args)
		return err
	}).Should(Succeed())
	vcpupid := strings.TrimSpace(strings.Trim(output, "\n"))
	tasksetcmd := "taskset -c -p " + vcpupid + " | cut -f2 -d:"
	args = []string{"/bin/bash", "-c", tasksetcmd}
	output, err = exec.ExecuteCommandOnPod(pod, "compute", args)
	Expect(err).ToNot(HaveOccurred())

	return strings.TrimSpace(output), err
}

func getPodCPUSet(pod *k8sv1.Pod) (output string, err error) {
	const (
		cgroupV1cpusetPath = "/sys/fs/cgroup/cpuset/cpuset.cpus"
		cgroupV2cpusetPath = "/sys/fs/cgroup/cpuset.cpus.effective"
	)

	output, err = exec.ExecuteCommandOnPod(
		pod,
		"compute",
		[]string{"cat", cgroupV2cpusetPath},
	)

	if err == nil {
		return
	}

	output, err = exec.ExecuteCommandOnPod(
		pod,
		"compute",
		[]string{"cat", cgroupV1cpusetPath},
	)

	return
}
