package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/emicklei/go-restful/v3"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"

	kubevirtcore "kubevirt.io/api/core"
	v1 "kubevirt.io/api/core/v1"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/kubevirt/fake"

	"kubevirt.io/kubevirt/pkg/instancetype"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
)

var _ = Describe("Instancetype expansion subresources", func() {
	const (
		vmName      = "test-vm"
		vmNamespace = "test-namespace"
		volumeName  = "volumeName"
	)

	var (
		vmClient            *kubecli.MockVirtualMachineInterface
		virtClient          *kubecli.MockKubevirtClient
		instancetypeMethods *testutils.MockInstancetypeMethods
		app                 *SubresourceAPIApp

		request  *restful.Request
		recorder *httptest.ResponseRecorder
		response *restful.Response

		vm *v1.VirtualMachine
	)

	BeforeEach(func() {
		ctrl := gomock.NewController(GinkgoT())
		vmClient = kubecli.NewMockVirtualMachineInterface(ctrl)
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		virtClient.EXPECT().GeneratedKubeVirtClient().Return(fake.NewSimpleClientset()).AnyTimes()
		virtClient.EXPECT().VirtualMachine(vmNamespace).Return(vmClient).AnyTimes()

		instancetypeMethods = testutils.NewMockInstancetypeMethods()

		kv := &v1.KubeVirt{
			ObjectMeta: k8smetav1.ObjectMeta{
				Name:      "kubevirt",
				Namespace: "kubevirt",
			},
			Spec: v1.KubeVirtSpec{
				Configuration: v1.KubeVirtConfiguration{
					DeveloperConfiguration: &v1.DeveloperConfiguration{},
				},
			},
			Status: v1.KubeVirtStatus{
				Phase: v1.KubeVirtPhaseDeployed,
			},
		}

		app = NewSubresourceAPIApp(virtClient, 0, nil, nil)
		app.instancetypeMethods = instancetypeMethods
		config, _, _ := testutils.NewFakeClusterConfigUsingKV(kv)
		app.clusterConfig = config

		request = restful.NewRequest(&http.Request{})
		recorder = httptest.NewRecorder()
		response = restful.NewResponse(recorder)
		response.SetRequestAccepts(restful.MIME_JSON)

		vm = &v1.VirtualMachine{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmName,
				Namespace: vmNamespace,
			},
			Spec: v1.VirtualMachineSpec{
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: v1.VirtualMachineInstanceSpec{
						Domain: v1.DomainSpec{},
						Volumes: []v1.Volume{{
							Name: volumeName,
						}},
					},
				},
			},
		}
	})

	testCommonFunctionality := func(callExpandSpecApi func(vm *v1.VirtualMachine) *httptest.ResponseRecorder, expectedStatusError int) {
		It("should return unchanged VM, if no instancetype and preference is assigned", func() {
			vm.Spec.Instancetype = nil

			recorder := callExpandSpecApi(vm)
			Expect(recorder.Code).To(Equal(http.StatusOK))

			responseVm := &v1.VirtualMachine{}
			Expect(json.NewDecoder(recorder.Body).Decode(responseVm)).To(Succeed())
			Expect(responseVm).To(Equal(vm))
		})

		It("should fail if VM points to nonexistent instancetype", func() {
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return nil, fmt.Errorf("instancetype does not exist")
			}

			vm.Spec.Instancetype = &v1.InstancetypeMatcher{
				Name: "nonexistent-instancetype",
			}

			recorder := callExpandSpecApi(vm)
			statusErr := ExpectStatusErrorWithCode(recorder, expectedStatusError)
			Expect(statusErr.Status().Message).To(ContainSubstring("instancetype does not exist"))
		})

		It("should fail if VM points to nonexistent preference", func() {
			instancetypeMethods.FindPreferenceSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
				return nil, fmt.Errorf("preference does not exist")
			}

			vm.Spec.Preference = &v1.PreferenceMatcher{
				Name: "nonexistent-preference",
			}

			recorder := callExpandSpecApi(vm)
			statusErr := ExpectStatusErrorWithCode(recorder, expectedStatusError)
			Expect(statusErr.Status().Message).To(ContainSubstring("preference does not exist"))
		})

		It("should apply instancetype to VM", func() {
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}

			cpu := &v1.CPU{Cores: 2}
			annotations := map[string]string{
				"annotations-1": "1",
				"annotations-2": "2",
			}

			instancetypeMethods.ApplyToVmiFunc = func(field *k8sfield.Path, instancetypespec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec, vmiMetadata *metav1.ObjectMeta) instancetype.Conflicts {
				vmiSpec.Domain.CPU = cpu
				vmiMetadata.Annotations = annotations
				return nil
			}

			vm.Spec.Instancetype = &v1.InstancetypeMatcher{
				Name: "test-instancetype",
			}

			expectedVm := vm.DeepCopy()

			Expect(webhooks.SetDefaultVirtualMachineInstanceSpec(app.clusterConfig, &expectedVm.Spec.Template.Spec)).To(Succeed())

			util.SetDefaultVolumeDisk(&expectedVm.Spec.Template.Spec)
			Expect(expectedVm.Spec.Template.Spec.Domain.Devices.Disks).To(HaveLen(1))
			Expect(expectedVm.Spec.Template.Spec.Domain.Devices.Disks[0].Name).To(Equal(volumeName))

			Expect(vmispec.SetDefaultNetworkInterface(app.clusterConfig, &expectedVm.Spec.Template.Spec)).To(Succeed())
			Expect(expectedVm.Spec.Template.Spec.Networks).To(HaveLen(1))
			Expect(expectedVm.Spec.Template.Spec.Networks[0].Name).To(Equal("default"))

			expectedVm.Spec.Template.Spec.Domain.CPU = cpu
			expectedVm.Spec.Template.ObjectMeta.Annotations = annotations

			recorder := callExpandSpecApi(vm)
			responseVm := &v1.VirtualMachine{}
			Expect(json.NewDecoder(recorder.Body).Decode(responseVm)).To(Succeed())

			Expect(responseVm.Spec.Template.ObjectMeta.Annotations).To(Equal(expectedVm.Spec.Template.ObjectMeta.Annotations))
			Expect(responseVm.Spec.Template.Spec).To(Equal(expectedVm.Spec.Template.Spec))
		})

		It("should apply preference to VM", func() {
			instancetypeMethods.FindPreferenceSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
				return &instancetypev1beta1.VirtualMachinePreferenceSpec{}, nil
			}

			machineType := "test-machine"
			instancetypeMethods.ApplyToVmiFunc = func(field *k8sfield.Path, instancetypespec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec, vmiMetadata *metav1.ObjectMeta) instancetype.Conflicts {
				vmiSpec.Domain.Machine = &v1.Machine{Type: machineType}
				return nil
			}

			vm.Spec.Preference = &v1.PreferenceMatcher{
				Name: "test-preference",
			}

			expectedVm := vm.DeepCopy()

			Expect(webhooks.SetDefaultVirtualMachineInstanceSpec(app.clusterConfig, &expectedVm.Spec.Template.Spec)).To(Succeed())

			util.SetDefaultVolumeDisk(&expectedVm.Spec.Template.Spec)
			Expect(expectedVm.Spec.Template.Spec.Domain.Devices.Disks).To(HaveLen(1))
			Expect(expectedVm.Spec.Template.Spec.Domain.Devices.Disks[0].Name).To(Equal(volumeName))

			Expect(vmispec.SetDefaultNetworkInterface(app.clusterConfig, &expectedVm.Spec.Template.Spec)).To(Succeed())
			Expect(expectedVm.Spec.Template.Spec.Networks).To(HaveLen(1))
			Expect(expectedVm.Spec.Template.Spec.Networks[0].Name).To(Equal("default"))
			expectedVm.Spec.Template.Spec.Domain.Machine = &v1.Machine{Type: machineType}

			recorder := callExpandSpecApi(vm)
			responseVm := &v1.VirtualMachine{}
			Expect(json.NewDecoder(recorder.Body).Decode(responseVm)).To(Succeed())

			Expect(responseVm.Spec.Template.Spec).To(Equal(expectedVm.Spec.Template.Spec))
		})

		It("should fail, if there is a conflict when applying instancetype", func() {
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}

			instancetypeMethods.ApplyToVmiFunc = func(field *k8sfield.Path, instancetypespec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec, vmiMetadata *metav1.ObjectMeta) instancetype.Conflicts {
				return instancetype.Conflicts{k8sfield.NewPath("spec", "template", "spec", "example", "path")}
			}

			recorder := callExpandSpecApi(vm)

			statusErr := ExpectStatusErrorWithCode(recorder, expectedStatusError)
			Expect(statusErr.Status().Message).To(ContainSubstring("cannot expand instancetype to VM"))
		})
	}

	Context("VirtualMachine expand-spec endpoint", func() {
		callExpandSpecApi := func(vm *v1.VirtualMachine) *httptest.ResponseRecorder {
			request.PathParameters()["name"] = vmName
			request.PathParameters()["namespace"] = vmNamespace

			vmClient.EXPECT().Get(context.Background(), vmName, gomock.Any()).Return(vm, nil).AnyTimes()

			app.ExpandSpecVMRequestHandler(request, response)
			return recorder
		}

		testCommonFunctionality(callExpandSpecApi, http.StatusInternalServerError)

		It("should fail if VM does not exist", func() {
			request.PathParameters()["name"] = "nonexistent-vm"
			request.PathParameters()["namespace"] = vmNamespace

			vmClient.EXPECT().Get(context.Background(), gomock.Any(), gomock.Any()).Return(nil, errors.NewNotFound(
				schema.GroupResource{
					Group:    kubevirtcore.GroupName,
					Resource: "VirtualMachine",
				},
				"",
			)).AnyTimes()

			app.ExpandSpecVMRequestHandler(request, response)
			statusErr := ExpectStatusErrorWithCode(recorder, http.StatusNotFound)
			Expect(statusErr.Status().Message).To(Equal("virtualmachine.kubevirt.io \"nonexistent-vm\" not found"))
		})
	})

	Context("expand-vm-spec endpoint", func() {
		callExpandSpecApi := func(vm *v1.VirtualMachine) *httptest.ResponseRecorder {
			request.PathParameters()["namespace"] = vmNamespace

			vmJson, err := json.Marshal(vm)
			Expect(err).ToNot(HaveOccurred())
			request.Request.Body = io.NopCloser(bytes.NewBuffer(vmJson))

			app.ExpandSpecRequestHandler(request, response)
			return recorder
		}

		testCommonFunctionality(callExpandSpecApi, http.StatusBadRequest)

		It("should fail if received invalid JSON", func() {
			request.PathParameters()["namespace"] = vmNamespace

			invalidJson := "this is invalid JSON {{{{"
			request.Request.Body = io.NopCloser(strings.NewReader(invalidJson))

			app.ExpandSpecRequestHandler(request, response)
			statusErr := ExpectStatusErrorWithCode(recorder, http.StatusBadRequest)
			Expect(statusErr.Status().Message).To(ContainSubstring("Can not unmarshal Request body to struct"))
		})

		It("should fail if received object is not a VirtualMachine", func() {
			request.PathParameters()["namespace"] = vmNamespace

			notVm := struct {
				StringField string `json:"stringField"`
				IntField    int    `json:"intField"`
			}{
				StringField: "test",
				IntField:    10,
			}

			jsonBytes, err := json.Marshal(notVm)
			Expect(err).ToNot(HaveOccurred())
			request.Request.Body = io.NopCloser(bytes.NewBuffer(jsonBytes))

			app.ExpandSpecRequestHandler(request, response)
			statusErr := ExpectStatusErrorWithCode(recorder, http.StatusBadRequest)
			Expect(statusErr.Status().Message).To(Equal("Object is not a valid VirtualMachine"))
		})

		It("should fail if endpoint namespace is empty", func() {
			request.PathParameters()["namespace"] = ""

			vmJson, err := json.Marshal(vm)
			Expect(err).ToNot(HaveOccurred())
			request.Request.Body = io.NopCloser(bytes.NewBuffer(vmJson))

			app.ExpandSpecRequestHandler(request, response)
			statusErr := ExpectStatusErrorWithCode(recorder, http.StatusBadRequest)
			Expect(statusErr.Status().Message).To(Equal("The request namespace must not be empty"))
		})

		It("should fail, if VM and endpoint namespace are different", func() {
			vm.Namespace = "madethisup"

			recorder = callExpandSpecApi(vm)
			statusErr := ExpectStatusErrorWithCode(recorder, http.StatusBadRequest)
			errMsg := fmt.Sprintf("VM namespace must be empty or %s", vmNamespace)
			Expect(statusErr.Status().Message).To(Equal(errMsg))
		})
	})
})
