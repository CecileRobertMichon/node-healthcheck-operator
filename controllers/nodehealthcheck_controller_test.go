package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/cluster"
	"github.com/medik8s/node-healthcheck-operator/controllers/mhc"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils"
)

var _ = Describe("Node Health Check CR", func() {

	Context("Defaults", func() {
		var underTest *v1alpha1.NodeHealthCheck

		BeforeEach(func() {
			underTest = &v1alpha1.NodeHealthCheck{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.NodeHealthCheckSpec{
					Selector: metav1.LabelSelector{},
					RemediationTemplate: &v1.ObjectReference{
						Kind:      "InfrastructureRemediationTemplate",
						Namespace: "default",
						Name:      "template",
					},
				},
			}
			err := k8sClient.Create(context.Background(), underTest)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			err := k8sClient.Delete(context.Background(), underTest)
			Expect(err).NotTo(HaveOccurred())
		})

		When("creating a resource", func() {
			It("it should have all default values set", func() {
				Expect(underTest.Namespace).To(BeEmpty())
				Expect(underTest.Spec.UnhealthyConditions).To(HaveLen(2))
				Expect(underTest.Spec.UnhealthyConditions[0].Type).To(Equal(v1.NodeReady))
				Expect(underTest.Spec.UnhealthyConditions[0].Status).To(Equal(v1.ConditionFalse))
				Expect(underTest.Spec.UnhealthyConditions[0].Duration).To(Equal(metav1.Duration{Duration: time.Minute * 5}))
				Expect(underTest.Spec.UnhealthyConditions[1].Type).To(Equal(v1.NodeReady))
				Expect(underTest.Spec.UnhealthyConditions[1].Status).To(Equal(v1.ConditionUnknown))
				Expect(underTest.Spec.UnhealthyConditions[1].Duration).To(Equal(metav1.Duration{Duration: time.Minute * 5}))
				Expect(underTest.Spec.MinHealthy.StrVal).To(Equal(intstr.FromString("51%").StrVal))
				Expect(underTest.Spec.Selector.MatchLabels).To(BeEmpty())
				Expect(underTest.Spec.Selector.MatchExpressions).To(BeEmpty())
			})
		})

		When("updating status", func() {
			It("succeeds updating only part of the fields", func() {
				Expect(underTest.Status).ToNot(BeNil())
				Expect(underTest.Status.HealthyNodes).To(Equal(0))
				patch := client.MergeFrom(underTest.DeepCopy())
				underTest.Status.HealthyNodes = 1
				underTest.Status.ObservedNodes = 6
				err := k8sClient.Status().Patch(context.Background(), underTest, patch)
				Expect(err).NotTo(HaveOccurred())
				Expect(underTest.Status.HealthyNodes).To(Equal(1))
				Expect(underTest.Status.ObservedNodes).To(Equal(6))
				Expect(underTest.Status.InFlightRemediations).To(BeNil())
			})
		})

	})

	Context("Validation", func() {
		var underTest *v1alpha1.NodeHealthCheck

		BeforeEach(func() {
			underTest = &v1alpha1.NodeHealthCheck{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.NodeHealthCheckSpec{
					RemediationTemplate: &v1.ObjectReference{
						Kind:      "InfrastructureRemediationTemplate",
						Namespace: "default",
						Name:      "template",
					},
				},
			}
		})

		AfterEach(func() {
			_ = k8sClient.Delete(context.Background(), underTest)
		})

		When("specifying an external remediation template", func() {
			It("should fail creation if empty", func() {
				underTest.Spec.RemediationTemplate = nil
				err := k8sClient.Create(context.Background(), underTest)
				Expect(err).To(HaveOccurred())
			})

			It("should succeed creation if a template CR doesn't exists", func() {
				err := k8sClient.Create(context.Background(), underTest)
				Expect(err).NotTo(HaveOccurred())
			})
		})
		When("specifying min healthy", func() {
			It("fails creation on percentage > 100%", func() {
				invalidPercentage := intstr.FromString("150%")
				underTest.Spec.MinHealthy = &invalidPercentage
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeTrue())
			})

			It("fails creation on negative number", func() {
				// This test does not work yet, because the "minimum" validation
				// of kubebuilder does not work for IntOrString.
				// Un-skip this as soon as this is supported.
				// For now negative minHealthy is validated during reconcile and will disable NHC,
				// see "minHealthy is negative" test further down.
				Skip("Does not work yet")
				invalidInt := intstr.FromInt(-10)
				underTest.Spec.MinHealthy = &invalidInt
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeTrue())
			})

			It("succeeds creation on percentage between 0%-100%", func() {
				validPercentage := intstr.FromString("30%")
				underTest.Spec.MinHealthy = &validPercentage
				err := k8sClient.Create(context.Background(), underTest)
				Expect(errors.IsInvalid(err)).To(BeFalse())
			})
		})
	})

	Context("Reconciliation", func() {
		var (
			underTest       *v1alpha1.NodeHealthCheck
			objects         []runtime.Object
			reconciler      NodeHealthCheckReconciler
			upgradeChecker  fakeClusterUpgradeChecker
			mhcChecker      mhc.DummyChecker
			reconcileError  error
			reconcileResult controllerruntime.Result
			getNHCError     error
		)

		var setupObjects = func(unhealthy int, healthy int) {
			objects = newNodes(unhealthy, healthy)
			underTest = newNodeHealthCheck()
			remediationTemplate := newRemediationTemplate()
			objects = append(objects, underTest, remediationTemplate)
		}

		JustBeforeEach(func() {
			client := fake.NewClientBuilder().WithRuntimeObjects(objects...).Build()
			reconciler = NodeHealthCheckReconciler{
				Client:                      client,
				Log:                         controllerruntime.Log.WithName("NHC Test Reconciler"),
				Scheme:                      scheme.Scheme,
				ClusterUpgradeStatusChecker: &upgradeChecker,
				MHCChecker:                  mhcChecker,
				Recorder:                    record.NewFakeRecorder(20),
			}
			reconcileResult, reconcileError = reconciler.Reconcile(
				context.Background(),
				controllerruntime.Request{NamespacedName: types.NamespacedName{Name: underTest.Name}})
			getNHCError = reconciler.Get(
				context.Background(),
				ctrlruntimeclient.ObjectKey{Namespace: underTest.Namespace, Name: underTest.Name},
				underTest)
		})

		When("few nodes are unhealthy and healthy nodes meet min healthy", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
			})

			It("create a remediation CR for each unhealthy node", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				cr := newRemediationCR("unhealthy-node-1")
				err := reconciler.Client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: cr.GetNamespace(), Name: cr.GetName()}, &cr)
				Expect(err).NotTo(HaveOccurred())
				Expect(cr.Object).To(ContainElement(map[string]interface{}{"size": "foo"}))
				Expect(cr.GetOwnerReferences()).
					To(ContainElement(metav1.OwnerReference{
						Kind:       underTest.Kind,
						APIVersion: underTest.APIVersion,
						Name:       underTest.Name,
						Controller: pointer.BoolPtr(false),
					}))
				Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(BeEmpty())
			})

			It("succeeds and correctly updates the status", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(getNHCError).NotTo(HaveOccurred())
				Expect(underTest.Status.HealthyNodes).To(Equal(2))
				Expect(underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
				Expect(underTest.Status.Conditions).To(ContainElement(
					And(
						HaveField("Type", v1alpha1.ConditionTypeDisabled),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Reason", v1alpha1.ConditionReasonEnabled),
					)))

			})

		})

		When("few nodes are unhealthy and healthy nodes above min healthy", func() {
			BeforeEach(func() {
				setupObjects(4, 3)
			})

			It("skips remediation - CR is not created, status updated correctly", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(getNHCError).NotTo(HaveOccurred())
				o := newRemediationCR("unhealthy-node-1")
				err := reconciler.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: o.GetNamespace(),
					Name: o.GetName()}, &o)
				Expect(errors.IsNotFound(err)).To(BeTrue())
				Expect(underTest.Status.HealthyNodes).To(Equal(3))
				Expect(underTest.Status.ObservedNodes).To(Equal(7))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})

		})

		When("few nodes become healthy", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				remediationCR := newRemediationCR("healthy-node-2")
				remediationCROther := newRemediationCR("healthy-node-1")
				refs := remediationCROther.GetOwnerReferences()
				refs[0].Name = "other"
				remediationCROther.SetOwnerReferences(refs)
				objects = append(objects, remediationCR.DeepCopy(), remediationCROther.DeepCopy())
			})

			It("deletes an existing remediation CR", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(getNHCError).NotTo(HaveOccurred())

				cr := newRemediationCR("unhealthy-node-1")
				err := reconciler.Client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: cr.GetNamespace(), Name: cr.GetName()}, &cr)
				Expect(err).NotTo(HaveOccurred())

				cr = newRemediationCR("healthy-node-2")
				err = reconciler.Client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: cr.GetNamespace(), Name: cr.GetName()}, &cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				// owned by other NHC, should not be deleted
				cr = newRemediationCR("healthy-node-1")
				err = reconciler.Client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: cr.GetNamespace(), Name: cr.GetName()}, &cr)
				Expect(err).NotTo(HaveOccurred())
			})

			It("updates the NHC status correctly", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(getNHCError).NotTo(HaveOccurred())
				Expect(underTest.Status.HealthyNodes).To(Equal(2))
				Expect(underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})
		})

		When("an old remediation cr exist", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				remediationCR := newRemediationCR("unhealthy-node-1")
				remediationCR.SetCreationTimestamp(metav1.Time{Time: time.Now().Add(-remediationCRAlertTimeout - 2*time.Minute)})
				objects = append(objects, remediationCR.DeepCopyObject())
			})

			It("an alert flag is set on remediation cr", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(getNHCError).NotTo(HaveOccurred())

				actualRemediationCR := new(unstructured.Unstructured)
				actualRemediationCR.SetKind(strings.TrimSuffix(underTest.Spec.RemediationTemplate.Kind, templateSuffix))
				actualRemediationCR.SetAPIVersion(underTest.Spec.RemediationTemplate.APIVersion)
				key := client.ObjectKey{Name: "unhealthy-node-1", Namespace: "default"}
				err := reconciler.Client.Get(context.Background(), key, actualRemediationCR)
				Expect(err).NotTo(HaveOccurred())
				Expect(actualRemediationCR.GetAnnotations()[oldRemediationCRAnnotationKey]).To(Equal("flagon"))
			})
		})

		When("remediation is needed but pauseRequests exists", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				underTest.Spec.PauseRequests = []string{"I'm an admin, asking you to stop remediating this group of nodes"}
			})

			It("should reconcile successfully", func() {
				Expect(reconcileError).ShouldNot(HaveOccurred())
			})

			It("skips remediation - CR is not created", func() {
				o := newRemediationCR("unhealthy-node-1")
				err := reconciler.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: o.GetNamespace(),
					Name: o.GetName()}, &o)
				Expect(errors.IsNotFound(err)).To(BeTrue())
			})

			It("updates the NHC status", func() {
				Expect(getNHCError).NotTo(HaveOccurred())
				Expect(underTest.Status.HealthyNodes).To(Equal(2))
				Expect(underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhasePaused))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})
		})

		When("Nodes are candidates for remediation and cluster is upgrading", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				upgradeChecker = fakeClusterUpgradeChecker{upgrading: true}
			})

			It("requeues reconciliation to 1 minute from now and updates status", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(reconcileResult.RequeueAfter).To(Equal(1 * time.Minute))
				Expect(underTest.Status.HealthyNodes).To(Equal(2))
				Expect(underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})

		})

		When("Nodes are candidates for remediation but remediation template is broken", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				underTest.Spec.RemediationTemplate.Name = "dummy"
			})

			It("should set corresponding condition", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
				Expect(underTest.Status.Reason).To(
					And(
						ContainSubstring("dummy"),
						ContainSubstring("not found"),
					))
				Expect(underTest.Status.Conditions).To(ContainElement(
					And(
						HaveField("Type", v1alpha1.ConditionTypeDisabled),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateNotFound),
					)))
			})
		})

		// Remove this and corresponding code when kubebuilder supports minimum on IntOrStr types
		// and don't skip earlier validation test anymore
		When("minHealthy is negative", func() {
			BeforeEach(func() {
				setupObjects(1, 2)
				underTest.Spec.RemediationTemplate.Name = "dummy"
				mh := intstr.FromInt(-10)
				underTest.Spec.MinHealthy = &mh
			})

			It("should set corresponding phase and condition", func() {
				Expect(reconcileError).NotTo(HaveOccurred())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
				Expect(underTest.Status.Reason).To(ContainSubstring("MinHealthy is negative"))
				Expect(underTest.Status.Conditions).To(ContainElement(
					And(
						HaveField("Type", v1alpha1.ConditionTypeDisabled),
						HaveField("Status", metav1.ConditionTrue),
						HaveField("Reason", v1alpha1.ConditionReasonDisabledInvalidConfig),
					)))
			})
		})
	})

	// TODO move to new suite in utils package
	Context("Controller Watches", func() {
		var (
			underTest1 *v1alpha1.NodeHealthCheck
			underTest2 *v1alpha1.NodeHealthCheck
			objects    []runtime.Object
			client     ctrlruntimeclient.Client
		)

		JustBeforeEach(func() {
			client = fake.NewClientBuilder().WithRuntimeObjects(objects...).Build()
		})

		When("a node changes status and is selectable by one NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				emptySelector, _ := metav1.ParseToLabelSelector("fooLabel=bar")
				underTest2.Spec.Selector = *emptySelector
				objects = append(objects, underTest1, underTest2)
			})

			It("creates a reconcile request", func() {
				handler := utils.NHCByNodeMapperFunc(client, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(1))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
			})
		})

		When("a node changes status and is selectable by the more 2 NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				objects = append(objects, underTest1, underTest2)
			})

			It("creates 2 reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(client, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(2))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest2.GetName()}}))
			})
		})
		When("a node changes status and there are no NHC objects", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10)
			})

			It("doesn't create reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(client, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(requests).To(BeEmpty())
			})
		})
	})
})

func newRemediationCR(nodeName string) unstructured.Unstructured {
	cr := unstructured.Unstructured{}
	cr.SetName(nodeName)
	cr.SetNamespace("default")
	cr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   TestRemediationCRD.Spec.Group,
		Version: TestRemediationCRD.Spec.Versions[0].Name,
		Kind:    TestRemediationCRD.Spec.Names.Kind,
	})
	cr.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: "remediation.medik8s.io/v1alpha1",
			Kind:       "NodeHealthCheck",
			Name:       "test",
		},
	})
	return cr
}

func newRemediationTemplate() runtime.Object {
	r := map[string]interface{}{
		"kind":       "InfrastructureRemediation",
		"apiVersion": "test.medik8s.io/v1alpha1",
		"metadata":   map[string]interface{}{},
		"spec": map[string]interface{}{
			"size": "foo",
		},
	}
	template := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": r,
			},
		},
	}
	template.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "test.medik8s.io",
		Version: "v1alpha1",
		Kind:    "InfrastructureRemediationTemplate",
	})
	template.SetGenerateName("remediation-template-name-")
	template.SetNamespace("default")
	template.SetName("template")
	return template.DeepCopyObject()
}

func newNodeHealthCheck() *v1alpha1.NodeHealthCheck {
	unhealthy := intstr.FromString("51%")
	return &v1alpha1.NodeHealthCheck{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NodeHealthCheck",
			APIVersion: "remediation.medik8s.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
		},
		Spec: v1alpha1.NodeHealthCheckSpec{
			Selector:   metav1.LabelSelector{},
			MinHealthy: &unhealthy,
			UnhealthyConditions: []v1alpha1.UnhealthyCondition{
				{
					Type:     v1.NodeReady,
					Status:   v1.ConditionFalse,
					Duration: metav1.Duration{Duration: time.Second * 300},
				},
			},
			RemediationTemplate: &v1.ObjectReference{
				Kind:       "InfrastructureRemediationTemplate",
				APIVersion: "test.medik8s.io/v1alpha1",
				Namespace:  "default",
				Name:       "template",
			},
		},
	}
}

func newNodes(unhealthy int, healthy int) []runtime.Object {
	o := make([]runtime.Object, 0, healthy+unhealthy)
	for i := unhealthy; i > 0; i-- {
		node := newNode(fmt.Sprintf("unhealthy-node-%d", i), v1.NodeReady, v1.ConditionFalse, time.Minute*10)
		o = append(o, node)
	}
	for i := healthy; i > 0; i-- {
		o = append(o, newNode(fmt.Sprintf("healthy-node-%d", i), v1.NodeReady, v1.ConditionTrue, time.Minute*10))
	}
	return o
}

func newNode(name string, t v1.NodeConditionType, s v1.ConditionStatus, d time.Duration) runtime.Object {
	return runtime.Object(
		&v1.Node{
			TypeMeta:   metav1.TypeMeta{Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.NodeStatus{
				Conditions: []v1.NodeCondition{
					{
						Type:               t,
						Status:             s,
						LastTransitionTime: metav1.Time{Time: time.Now().Add(-d)},
					},
				},
			},
		})
}

var TestRemediationCRD = &apiextensions.CustomResourceDefinition{
	TypeMeta: metav1.TypeMeta{
		APIVersion: apiextensions.SchemeGroupVersion.String(),
		Kind:       "CustomResourceDefinition",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "infrastructureremediations.medik8s.io",
	},
	Spec: apiextensions.CustomResourceDefinitionSpec{
		Group: "test.medik8s.io",
		Scope: apiextensions.NamespaceScoped,
		Names: apiextensions.CustomResourceDefinitionNames{
			Kind:   "InfrastructureRemediation",
			Plural: "infrastructureremediations",
		},
		Versions: []apiextensions.CustomResourceDefinitionVersion{
			{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Subresources: &apiextensions.CustomResourceSubresources{
					Status: &apiextensions.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensions.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensions.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensions.JSONSchemaProps{
							"spec": {
								Type:                   "object",
								XPreserveUnknownFields: pointer.BoolPtr(true),
							},
							"status": {
								Type:                   "object",
								XPreserveUnknownFields: pointer.BoolPtr(true),
							},
						},
					},
				},
			},
		},
	},
}

type fakeClusterUpgradeChecker struct {
	upgrading bool
	err       error
}

// force implementation of interface
var _ cluster.UpgradeChecker = fakeClusterUpgradeChecker{}

func (c fakeClusterUpgradeChecker) Check() (bool, error) {
	return c.upgrading, c.err
}
