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

package virt_controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rhobs/operator-observability-toolkit/pkg/operatormetrics"
	appsv1 "k8s.io/api/apps/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k6tv1 "kubevirt.io/api/core/v1"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"

	"kubevirt.io/kubevirt/pkg/instancetype/apply"
	"kubevirt.io/kubevirt/pkg/instancetype/find"
	preferencefind "kubevirt.io/kubevirt/pkg/instancetype/preference/find"
	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = BeforeSuite(func() {
})

var _ = Describe("VMI Stats Collector", func() {
	clusterConfig, _, _ = testutils.NewFakeClusterConfigUsingKV(&k6tv1.KubeVirt{})

	Context("VMI info", func() {
		setupTestCollector()

		It("should handle no VMIs", func() {
			cr := reportVmisStats([]*k6tv1.VirtualMachineInstance{})
			Expect(cr).To(BeEmpty())
		})

		It("should handle different VMI phases", func() {
			vmis := []*k6tv1.VirtualMachineInstance{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "running#0",
						Annotations: map[string]string{
							annotationPrefix + "os":       "centos8",
							annotationPrefix + "workload": "server",
							annotationPrefix + "flavor":   "tiny",
						},
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						Phase: "Running",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "running#1",
						Annotations: map[string]string{
							annotationPrefix + "os":       "centos8",
							annotationPrefix + "workload": "server",
							annotationPrefix + "flavor":   "tiny",
						},
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						Phase: "Running",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pending#0",
						Annotations: map[string]string{
							annotationPrefix + "os":       "fedora33",
							annotationPrefix + "workload": "workstation",
							annotationPrefix + "flavor":   "large",
						},
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						Phase: "Pending",
						GuestOSInfo: k6tv1.VirtualMachineInstanceGuestOSInfo{
							KernelRelease: "6.5.6-300.fc39.x86_64",
							Machine:       "x86_64",
							Name:          "Fedora Linux",
							VersionID:     "39",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "scheduling#0",
						Annotations: map[string]string{
							annotationPrefix + "os":       "centos7",
							annotationPrefix + "workload": "server",
							annotationPrefix + "flavor":   "medium",
							annotationPrefix + "dummy":    "dummy",
						},
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						Phase: "Scheduling",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "scheduling#1",
						Annotations: map[string]string{
							annotationPrefix + "os":       "centos7",
							annotationPrefix + "workload": "server",
							annotationPrefix + "flavor":   "medium",
							annotationPrefix + "phase":    "dummy",
						},
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						Phase: "Scheduling",
					},
				},
			}

			var crs []operatormetrics.CollectorResult
			for _, vmi := range vmis {
				crs = append(crs, collectVMIInfo(vmi))
			}

			Expect(crs).To(HaveLen(5))

			for i, cr := range crs {
				Expect(cr).ToNot(BeNil())
				Expect(cr.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_info"))
				Expect(cr.Value).To(BeEquivalentTo(1))
				Expect(cr.Labels).To(HaveLen(17))

				Expect(cr.Labels[3]).To(Equal(getVMIPhase(vmis[i])))
				os, workload, flavor := getSystemInfoFromAnnotations(vmis[i].Annotations)
				Expect(cr.Labels[4]).To(Equal(os))
				Expect(cr.Labels[5]).To(Equal(workload))
				Expect(cr.Labels[6]).To(Equal(flavor))
				Expect(cr.Labels[16]).To(Equal(getVMIPod(vmis[i])))
			}
		})

		It("should update the vmi_pod label correctly after migration", func() {
			originalPod := &k8sv1.Pod{
				ObjectMeta: newPodMetaForInformer("virt-launcher-originalpod", "test-ns", "test-vmi-uid"),
				Spec: k8sv1.PodSpec{
					NodeName: "initial-node",
				},
				Status: k8sv1.PodStatus{
					Phase: k8sv1.PodRunning,
				},
			}
			targetPod := &k8sv1.Pod{
				ObjectMeta: newPodMetaForInformer("virt-launcher-targetpod", "test-ns", "test-vmi-uid"),
				Spec: k8sv1.PodSpec{
					NodeName: "target-node",
				},
				Status: k8sv1.PodStatus{
					Phase: k8sv1.PodRunning,
				},
			}

			_ = indexers.KVPod.Add(originalPod)
			_ = indexers.KVPod.Add(targetPod)

			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "test-vmi-uid",
				},
				Status: k6tv1.VirtualMachineInstanceStatus{
					Phase:    "Running",
					NodeName: "target-node",
					MigrationState: &k6tv1.VirtualMachineInstanceMigrationState{
						TargetPod: "virt-launcher-targetpod",
						Completed: true,
						Failed:    false,
					},
				},
			}

			cr := collectVMIInfo(vmi)

			Expect(cr).ToNot(BeNil())
			Expect(cr.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_info"))
			Expect(cr.Value).To(BeEquivalentTo(1))
			Expect(cr.Labels).To(HaveLen(17))
			Expect(cr.Labels[16]).To(Equal("virt-launcher-targetpod"))
		})

		It("should return the original pod when migration failed", func() {
			originalPod := &k8sv1.Pod{
				ObjectMeta: newPodMetaForInformer("virt-launcher-originalpod", "test-ns", "test-vmi-uid"),
				Spec: k8sv1.PodSpec{
					NodeName: "initial-node",
				},
				Status: k8sv1.PodStatus{
					Phase: k8sv1.PodRunning,
				},
			}
			targetPod := &k8sv1.Pod{
				ObjectMeta: newPodMetaForInformer("virt-launcher-targetpod", "test-ns", "test-vmi-uid"),
				Spec: k8sv1.PodSpec{
					NodeName: "target-node",
				},
				Status: k8sv1.PodStatus{
					Phase: k8sv1.PodRunning,
				},
			}

			_ = indexers.KVPod.Add(originalPod)
			_ = indexers.KVPod.Add(targetPod)

			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "test-vmi-uid",
				},
				Status: k6tv1.VirtualMachineInstanceStatus{
					Phase:    "Running",
					NodeName: "initial-node",
					MigrationState: &k6tv1.VirtualMachineInstanceMigrationState{
						TargetPod: "virt-launcher-targetpod",
						Completed: true,
						Failed:    true,
					},
				},
			}

			cr := collectVMIInfo(vmi)

			Expect(cr).ToNot(BeNil())
			Expect(cr.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_info"))
			Expect(cr.Value).To(BeEquivalentTo(1))
			Expect(cr.Labels).To(HaveLen(17))
			Expect(cr.Labels[16]).To(Equal("virt-launcher-originalpod"))
		})

		DescribeTable("should show instance type value correctly", func(instanceTypeAnnotationKey string, instanceType string, expected string) {
			annotations := map[string]string{}
			if instanceType != "" {
				annotations[instanceTypeAnnotationKey] = instanceType
			}

			vmis := []*k6tv1.VirtualMachineInstance{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "running",
						Namespace:   "test-ns",
						Annotations: annotations,
					},
				},
			}

			var crs []operatormetrics.CollectorResult
			for _, vmi := range vmis {
				crs = append(crs, collectVMIInfo(vmi))
			}
			Expect(crs).To(HaveLen(1), "Expected 1 metric")

			cr := crs[0]
			Expect(cr).ToNot(BeNil())
			Expect(cr.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_info"))
			Expect(cr.Value).To(BeEquivalentTo(1))
			Expect(cr.Labels).To(HaveLen(17))
			Expect(cr.Labels[7]).To(Equal(expected))
		},
			Entry("with no instance type expect empty string", k6tv1.InstancetypeAnnotation, "", ""),
			Entry("with managed instance type expect its name", k6tv1.InstancetypeAnnotation, "i-managed", "i-managed"),
			Entry("with custom instance type expect <other>", k6tv1.InstancetypeAnnotation, "i-unmanaged", "<other>"),
			Entry("with no cluster instance type expect empty string", k6tv1.ClusterInstancetypeAnnotation, "", ""),
			Entry("with managed cluster instance type expect its name", k6tv1.ClusterInstancetypeAnnotation, "ci-managed", "ci-managed"),
			Entry("with custom cluster instance type expect <other>", k6tv1.ClusterInstancetypeAnnotation, "ci-unmanaged", "<other>"),
		)

		DescribeTable("should show preference value correctly", func(preferenceAnnotationKey string, preference string, expected string) {
			annotations := map[string]string{}
			if preference != "" {
				annotations[preferenceAnnotationKey] = preference
			}

			vmis := []*k6tv1.VirtualMachineInstance{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "running",
						Namespace:   "test-ns",
						Annotations: annotations,
					},
				},
			}

			var crs []operatormetrics.CollectorResult
			for _, vmi := range vmis {
				crs = append(crs, collectVMIInfo(vmi))
			}
			Expect(crs).To(HaveLen(1), "Expected 1 metric")

			cr := crs[0]

			Expect(cr.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_info"))
			Expect(cr.Value).To(BeEquivalentTo(1))
			Expect(cr.Labels).To(HaveLen(17))
			Expect(cr.Labels[8]).To(Equal(expected))
		},
			Entry("with no preference expect empty string", k6tv1.PreferenceAnnotation, "", ""),
			Entry("with managed preference expect its name", k6tv1.PreferenceAnnotation, "p-managed", "p-managed"),
			Entry("with custom preference expect <other>", k6tv1.PreferenceAnnotation, "p-unmanaged", "<other>"),
			Entry("with no cluster preference expect empty string", k6tv1.ClusterPreferenceAnnotation, "", ""),
			Entry("with managed cluster preference expect its name", k6tv1.ClusterPreferenceAnnotation, "cp-managed", "cp-managed"),
			Entry("with custom cluster preference expect <other>", k6tv1.ClusterPreferenceAnnotation, "cp-unmanaged", "<other>"),
		)
	})

	Context("VMI Eviction blocker", func() {

		liveMigrateEvictPolicy := k6tv1.EvictionStrategyLiveMigrate
		DescribeTable("Add eviction alert metrics", func(evictionPolicy *k6tv1.EvictionStrategy, migrateCondStatus k8sv1.ConditionStatus, expectedVal float64) {
			vmiInformer, _ := testutils.NewFakeInformerFor(&k6tv1.VirtualMachineInstance{})
			stores.VMI = vmiInformer.GetStore()

			ch := make(chan prometheus.Metric, 1)
			defer close(ch)

			vmi := createVMIForEviction(evictionPolicy, migrateCondStatus)

			evictionBlockerResultMetric := getEvictionBlocker(vmi)
			Expect(evictionBlockerResultMetric).ToNot(BeNil())
			Expect(evictionBlockerResultMetric.Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_non_evictable"))
			Expect(evictionBlockerResultMetric.Value).To(BeEquivalentTo(expectedVal))
		},
			Entry("VMI Eviction policy set to LiveMigration and vm is not migratable", &liveMigrateEvictPolicy, k8sv1.ConditionFalse, 1.0),
			Entry("VMI Eviction policy set to LiveMigration and vm migratable status is not known", &liveMigrateEvictPolicy, k8sv1.ConditionUnknown, 1.0),
			Entry("VMI Eviction policy set to LiveMigration and vm is migratable", &liveMigrateEvictPolicy, k8sv1.ConditionTrue, 0.0),
			Entry("VMI Eviction policy is not set and vm is not migratable", nil, k8sv1.ConditionFalse, 0.0),
			Entry("VMI Eviction policy is not set and vm is migratable", nil, k8sv1.ConditionTrue, 0.0),
			Entry("VMI Eviction policy is not set and vm migratable status is not known", nil, k8sv1.ConditionUnknown, 0.0),
		)
	})

	Context("VMI Interfaces info", func() {
		DescribeTable("kubevirt_vmi_status_addresses metrics", func(ifaceValues [][]string) {
			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "testvmi",
				},
				Status: k6tv1.VirtualMachineInstanceStatus{
					NodeName:   "testNode",
					Interfaces: interfacesFor(ifaceValues),
				},
			}

			metrics := collectVMIInterfacesInfo(vmi)
			Expect(metrics).To(HaveLen(len(ifaceValues)))

			for i, labelValues := range ifaceValues {
				values := append([]string{"testNode", "test-ns", "testvmi"}, labelValues...)
				Expect(metrics[i].Labels).To(Equal(values))
			}
		},
			Entry("no interfaces", [][]string{}),
			Entry("one interface", [][]string{{"default", "", "192.168.1.2", "ExternalInterface"}}),
			Entry("two interfaces", [][]string{
				{"networkA", "", "170.170.170.170", "ExternalInterface"},
				{"networkB", "", "180.180.180.180", "ExternalInterface"},
			}),
		)

		It("should create metric for interfaces with empty name, but with interface name", func() {
			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "testvmi",
				},
				Status: k6tv1.VirtualMachineInstanceStatus{
					NodeName: "testNode",
					Interfaces: []k6tv1.VirtualMachineInstanceNetworkInterface{
						{
							InfoSource:    "guest-agent",
							InterfaceName: "br-int",
							MAC:           "00:00:00:00:00:01",
						},
						{
							InfoSource:    "guest-agent",
							InterfaceName: "ovs-system",
							MAC:           "00:00:00:00:00:02",
						},
					},
				},
			}

			metrics := collectVMIInterfacesInfo(vmi)
			Expect(metrics).To(HaveLen(2))
			Expect(metrics[0].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "", "br-int", "", "SystemInterface"}))
			Expect(metrics[1].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "", "ovs-system", "", "SystemInterface"}))
		})

		It("should not create metric for an interface with empty IP address, name and interface name", func() {
			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "testvmi",
				},
				Status: k6tv1.VirtualMachineInstanceStatus{
					NodeName: "testNode",
					Interfaces: []k6tv1.VirtualMachineInstanceNetworkInterface{
						{
							InfoSource:    "domain, guest-agent, multus-status",
							Name:          "net-0",
							InterfaceName: "br-ex",
							IP:            "10.11.126.126",
						},
						{
							InfoSource:    "guest-agent",
							InterfaceName: "ovs-system",
						},
						{
							InfoSource:    "guest-agent",
							InterfaceName: "br-int",
						},
						{
							InfoSource: "guest-agent",
						},
					},
				},
			}

			metrics := collectVMIInterfacesInfo(vmi)
			Expect(metrics).To(HaveLen(3))
			Expect(metrics[0].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "net-0", "br-ex", "10.11.126.126", "ExternalInterface"}))
			Expect(metrics[1].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "", "ovs-system", "", "SystemInterface"}))
			Expect(metrics[2].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "", "br-int", "", "SystemInterface"}))
		})
	})

	Context("VMI migration start and end time metrics", func() {
		now := metav1.Unix(1000, 0)
		nowFloatValue := float64(now.Unix())

		Describe("kubevirt_vmi_migration_start_time and kubevirt_vmi_migration_end_time metrics", func() {
			It("should not create migration metrics for a VMI with no migration state", func() {
				vmi := &k6tv1.VirtualMachineInstance{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-ns",
						Name:      "testvmi",
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						NodeName: "testNode",
					},
				}

				metrics := collectVMIMigrationTime(vmi)
				Expect(metrics).To(BeEmpty())
			})

			It("should create kubevirt_vmi_migration_start_time metric for a migration in progress", func() {
				vmi := &k6tv1.VirtualMachineInstance{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-ns",
						Name:      "testvmi",
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						NodeName: "testNode",
						MigrationState: &k6tv1.VirtualMachineInstanceMigrationState{
							MigrationUID:   "test-migration-uid",
							StartTimestamp: &now,
						},
					},
				}

				metrics := collectVMIMigrationTime(vmi)
				Expect(metrics).To(HaveLen(1))

				Expect(metrics[0].Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_migration_start_time"))
				Expect(metrics[0].Value).To(BeEquivalentTo(nowFloatValue))
				Expect(metrics[0].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "test-migration"}))
			})

			It("should create kubevirt_vmi_migration_end_time metric for a completedmigration", func() {
				vmi := &k6tv1.VirtualMachineInstance{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-ns",
						Name:      "testvmi",
					},
					Status: k6tv1.VirtualMachineInstanceStatus{
						NodeName: "testNode",
						MigrationState: &k6tv1.VirtualMachineInstanceMigrationState{
							MigrationUID:   "test-migration-uid",
							StartTimestamp: &now,
							EndTimestamp:   &now,
							Completed:      true,
							Failed:         false,
						},
					},
				}

				metrics := collectVMIMigrationTime(vmi)
				Expect(metrics).To(HaveLen(2))

				Expect(metrics[0].Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_migration_start_time"))
				Expect(metrics[0].Value).To(BeEquivalentTo(nowFloatValue))
				Expect(metrics[0].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "test-migration"}))

				Expect(metrics[1].Metric.GetOpts().Name).To(ContainSubstring("kubevirt_vmi_migration_end_time"))
				Expect(metrics[1].Value).To(BeEquivalentTo(nowFloatValue))
				Expect(metrics[1].Labels).To(Equal([]string{"testNode", "test-ns", "testvmi", "test-migration", "succeeded"}))
			})
		})
	})

	Context("VMI vNIC info", func() {
		It("should collect kubevirt_vmi_vnic_info metric with correct labels", func() {
			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-vmi",
				},
				Spec: k6tv1.VirtualMachineInstanceSpec{
					Domain: k6tv1.DomainSpec{
						Devices: k6tv1.Devices{
							Interfaces: []k6tv1.Interface{
								{
									Name: "iface1",
									InterfaceBindingMethod: k6tv1.InterfaceBindingMethod{
										Bridge: &k6tv1.InterfaceBridge{},
									},
									Model: "virtio",
								},
								{
									Name: "iface2",
									InterfaceBindingMethod: k6tv1.InterfaceBindingMethod{
										Masquerade: &k6tv1.InterfaceMasquerade{},
									},
									Model: "e1000e",
								},
								{
									Name: "iface3",
									InterfaceBindingMethod: k6tv1.InterfaceBindingMethod{
										SRIOV: &k6tv1.InterfaceSRIOV{},
									},
								},
								{
									Name:    "iface4",
									Binding: &k6tv1.PluginBinding{Name: "custom-plugin"},
								},
							},
						},
					},
					Networks: []k6tv1.Network{
						{
							Name:          "iface1",
							NetworkSource: k6tv1.NetworkSource{Pod: &k6tv1.PodNetwork{}},
						},
						{
							Name:          "iface2",
							NetworkSource: k6tv1.NetworkSource{Pod: &k6tv1.PodNetwork{}},
						},
						{
							Name:          "iface3",
							NetworkSource: k6tv1.NetworkSource{Multus: &k6tv1.MultusNetwork{NetworkName: "multus-net"}},
						},
						{
							Name:          "iface4",
							NetworkSource: k6tv1.NetworkSource{Multus: &k6tv1.MultusNetwork{NetworkName: "custom-net"}},
						},
					},
				},
			}

			metrics := CollectVmisVnicInfo(vmi)
			Expect(metrics).To(HaveLen(4))

			Expect(metrics[0].Labels).To(Equal([]string{"test-vmi", "test-ns", "iface1", "core", "pod networking", "bridge", "virtio"}))
			Expect(metrics[1].Labels).To(Equal([]string{"test-vmi", "test-ns", "iface2", "core", "pod networking", "masquerade", "e1000e"}))
			Expect(metrics[2].Labels).To(Equal([]string{"test-vmi", "test-ns", "iface3", "core", "multus-net", "sriov", "<none>"}))
			Expect(metrics[3].Labels).To(Equal([]string{"test-vmi", "test-ns", "iface4", "plugin", "custom-net", "custom-plugin", "<none>"}))
		})
		It("should not collect kubevirt_vmi_vnic_info metric when interface name is not matching network name", func() {
			vmi := &k6tv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-vmi",
				},
				Spec: k6tv1.VirtualMachineInstanceSpec{
					Domain: k6tv1.DomainSpec{
						Devices: k6tv1.Devices{
							Interfaces: []k6tv1.Interface{
								{
									Name: "iface1",
									InterfaceBindingMethod: k6tv1.InterfaceBindingMethod{
										Bridge: &k6tv1.InterfaceBridge{},
									},
								},
							},
						},
					},
					Networks: []k6tv1.Network{
						{
							Name:          "iface2",
							NetworkSource: k6tv1.NetworkSource{Pod: &k6tv1.PodNetwork{}},
						},
					},
				},
			}

			metrics := CollectVmisVnicInfo(vmi)
			Expect(metrics).To(BeEmpty())
		})
	})
})

func interfacesFor(values [][]string) []k6tv1.VirtualMachineInstanceNetworkInterface {
	interfaces := make([]k6tv1.VirtualMachineInstanceNetworkInterface, len(values))
	for i, v := range values {
		interfaces[i] = k6tv1.VirtualMachineInstanceNetworkInterface{
			Name:          v[0],
			InterfaceName: v[1],
			IP:            v[2],
		}
	}
	return interfaces
}

func createVMIForEviction(evictionStrategy *k6tv1.EvictionStrategy, migratableCondStatus k8sv1.ConditionStatus) *k6tv1.VirtualMachineInstance {
	vmi := &k6tv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "testvmi",
		},
		Status: k6tv1.VirtualMachineInstanceStatus{
			NodeName: "testNode",
		},
	}

	if migratableCondStatus != k8sv1.ConditionUnknown {
		vmi.Status.Conditions = []k6tv1.VirtualMachineInstanceCondition{
			{
				Type:   k6tv1.VirtualMachineInstanceIsMigratable,
				Status: migratableCondStatus,
			},
		}
	}

	vmi.Spec.EvictionStrategy = evictionStrategy

	return vmi
}

func setupTestCollector() {
	instanceTypeInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineInstancetype{})
	clusterInstanceTypeInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineClusterInstancetype{})
	preferenceInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachinePreference{})
	clusterPreferenceInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineClusterPreference{})
	controllerRevisionInformer, _ := testutils.NewFakeInformerFor(&appsv1.ControllerRevision{})

	_ = instanceTypeInformer.GetStore().Add(&instancetypev1beta1.VirtualMachineInstancetype{
		ObjectMeta: newObjectMetaForInstancetypes("i-managed", "test-ns", "kubevirt.io"),
	})
	_ = instanceTypeInformer.GetStore().Add(&instancetypev1beta1.VirtualMachineInstancetype{
		ObjectMeta: newObjectMetaForInstancetypes("i-unmanaged", "test-ns", "some-user"),
	})

	_ = clusterInstanceTypeInformer.GetStore().Add(&instancetypev1beta1.VirtualMachineClusterInstancetype{
		ObjectMeta: newObjectMetaForInstancetypes("ci-managed", "", "kubevirt.io"),
		Spec: instancetypev1beta1.VirtualMachineInstancetypeSpec{
			CPU: instancetypev1beta1.CPUInstancetype{
				Guest: 2,
			},
			Memory: instancetypev1beta1.MemoryInstancetype{
				Guest: *resource.NewQuantity(2048, resource.BinarySI),
			},
		},
	})
	_ = clusterInstanceTypeInformer.GetStore().Add(&instancetypev1beta1.VirtualMachineClusterInstancetype{
		ObjectMeta: newObjectMetaForInstancetypes("ci-unmanaged", "", ""),
	})

	_ = preferenceInformer.GetStore().Add(&instancetypev1beta1.VirtualMachinePreference{
		ObjectMeta: newObjectMetaForInstancetypes("p-managed", "test-ns", "kubevirt.io"),
	})
	_ = preferenceInformer.GetStore().Add(&instancetypev1beta1.VirtualMachinePreference{
		ObjectMeta: newObjectMetaForInstancetypes("p-unmanaged", "test-ns", "some-vendor.com"),
	})

	_ = clusterPreferenceInformer.GetStore().Add(&instancetypev1beta1.VirtualMachineClusterPreference{
		ObjectMeta: newObjectMetaForInstancetypes("cp-managed", "", "kubevirt.io"),
	})

	stores = &Stores{
		Instancetype:        instanceTypeInformer.GetStore(),
		ClusterInstancetype: clusterInstanceTypeInformer.GetStore(),
		Preference:          preferenceInformer.GetStore(),
		ClusterPreference:   clusterPreferenceInformer.GetStore(),
		ControllerRevision:  controllerRevisionInformer.GetStore(),
	}

	vmApplier = apply.NewVMApplier(
		find.NewSpecFinder(
			instanceTypeInformer.GetStore(),
			clusterInstanceTypeInformer.GetStore(),
			controllerRevisionInformer.GetStore(),
			nil,
		),
		preferencefind.NewSpecFinder(
			preferenceInformer.GetStore(),
			clusterPreferenceInformer.GetStore(),
			controllerRevisionInformer.GetStore(),
			nil,
		),
	)

	indexers = &Indexers{}

	// Pod informer
	kvPodInformer, _ := testutils.NewFakeInformerFor(&k8sv1.Pod{})
	indexers.KVPod = kvPodInformer.GetIndexer()

	_ = indexers.KVPod.Add(&k8sv1.Pod{
		ObjectMeta: newPodMetaForInformer("virt-launcher-testpod", "test-ns", "test-vmi-uid"),
	})

	// VMI Migration informer
	vmiMigrationInformer, _ := testutils.NewFakeInformerFor(&k6tv1.VirtualMachineInstanceMigration{})
	indexers.VMIMigration = vmiMigrationInformer.GetIndexer()

	_ = indexers.VMIMigration.Add(&k6tv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration",
			Namespace: "test-ns",
			UID:       "test-migration-uid",
		},
	})
}

func newObjectMetaForInstancetypes(name, namespace, vendor string) metav1.ObjectMeta {
	om := metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{instancetypeVendorLabel: vendor},
	}

	if namespace != "" {
		om.Namespace = namespace
	}

	return om
}

func newPodMetaForInformer(name, namespace, createdByUID string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels:    map[string]string{"kubevirt.io/created-by": createdByUID},
	}
}
