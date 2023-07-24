package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"

	"github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils"
)

const (
	unhealthyConditionDuration = 10 * time.Second
	nodeUnhealthyIn            = 5 * time.Second
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
				Expect(underTest.Status.HealthyNodes).To(BeNil())
				patch := client.MergeFrom(underTest.DeepCopy())
				underTest.Status.HealthyNodes = pointer.Int(1)
				underTest.Status.ObservedNodes = pointer.Int(6)
				err := k8sClient.Status().Patch(context.Background(), underTest, patch)
				Expect(err).NotTo(HaveOccurred())
				Expect(*underTest.Status.HealthyNodes).To(Equal(1))
				Expect(*underTest.Status.ObservedNodes).To(Equal(6))
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
				// For now negative minHealthy is validated via webhook.
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

	createObjects := func(objects ...client.Object) {
		for _, obj := range objects {
			Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		}
	}

	deleteObjects := func(objects ...client.Object) {
		for _, obj := range objects {
			// ignore errors, CRs might be deleted by reconcile
			_ = k8sClient.Delete(context.Background(), obj)
		}
	}

	Context("Reconciliation", func() {
		var (
			underTest *v1alpha1.NodeHealthCheck
			objects   []client.Object
		)

		setupObjects := func(unhealthy int, healthy int, unhealthyNow bool) {
			objects = newNodes(unhealthy, healthy, false, unhealthyNow)
			objects = append(objects, underTest)
		}

		BeforeEach(func() {
			underTest = newNodeHealthCheck()
		})

		JustBeforeEach(func() {
			createObjects(objects...)
			// give the reconciler some time
			time.Sleep(2 * time.Second)
			// get updated NHC
			Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
		})

		AfterEach(func() {
			// delete all created objects
			deleteObjects(objects...)

			// delete all remediation CRs
			var remediationKind string
			if underTest.Spec.RemediationTemplate != nil {
				remediationKind = underTest.Spec.RemediationTemplate.Kind
			} else {
				remediationKind = underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind
			}
			if remediationKind != "dummyTemplate" {
				cr := newRemediationCR("", underTest)
				crList := &unstructured.UnstructuredList{Object: cr.Object}
				Expect(k8sClient.List(context.Background(), crList)).To(Succeed())
				for _, item := range crList.Items {
					Expect(k8sClient.Delete(context.Background(), &item)).To(Succeed())
				}
			}

			// let thing settle a bit
			time.Sleep(1 * time.Second)
		})

		testReconcile := func() {

			When("Nodes are candidates for remediation but remediation template is broken", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)

					if underTest.Spec.RemediationTemplate != nil {
						underTest.Spec.RemediationTemplate.Kind = "dummyTemplate"
					} else {
						underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind = "dummyTemplate"
					}
				})

				It("should set corresponding condition", func() {
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
					Expect(underTest.Status.Reason).To(
						And(
							ContainSubstring("failed to get"),
							ContainSubstring("dummyTemplate"),
						))
					Expect(underTest.Status.Conditions).To(ContainElement(
						And(
							HaveField("Type", v1alpha1.ConditionTypeDisabled),
							HaveField("Status", metav1.ConditionTrue),
							HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateNotFound),
						)))
				})
			})

			Context("Machine owners", func() {
				When("Metal3RemediationTemplate is in wrong namespace", func() {

					BeforeEach(func() {
						setupObjects(1, 2, true)

						// set metal3 template
						if underTest.Spec.RemediationTemplate != nil {
							underTest.Spec.RemediationTemplate.Kind = "Metal3RemediationTemplate"
							underTest.Spec.RemediationTemplate.Name = "nok"
							underTest.Spec.RemediationTemplate.Namespace = "default"
						} else {
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind = "Metal3RemediationTemplate"
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Name = "nok"
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Namespace = "default"
						}
					})

					It("should be disabled", func() {
						Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
						Expect(underTest.Status.Reason).To(
							ContainSubstring("Metal3RemediationTemplate must be in the openshift-machine-api namespace"),
						)
						Expect(underTest.Status.Conditions).To(ContainElement(
							And(
								HaveField("Type", v1alpha1.ConditionTypeDisabled),
								HaveField("Status", metav1.ConditionTrue),
								HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateInvalid),
							)))
					})
				})
			})

			When("few nodes are unhealthy and healthy nodes meet min healthy", func() {
				BeforeEach(func() {
					setupObjects(1, 2, false)
				})

				It("create a remediation CR for each unhealthy node and updates status", func() {
					cr := newRemediationCR("unhealthy-worker-node-1", underTest)
					// first call should fail, because the node gets unready in a few seconds only
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())
					// wait until nodes are unhealthy
					time.Sleep(nodeUnhealthyIn)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.Object).To(ContainElement(map[string]interface{}{"size": "foo"}))
					Expect(cr.GetOwnerReferences()).
						To(ContainElement(
							And(
								// Kind and API version aren't set on underTest, envtest issue...
								// Controller is empty for HaveField because false is the zero value?
								HaveField("Name", underTest.Name),
								HaveField("UID", underTest.UID),
							),
						))
					Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(BeEmpty())

					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
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

			When("few nodes are unhealthy and healthy nodes below min healthy", func() {
				BeforeEach(func() {
					setupObjects(4, 3, true)
				})

				It("skips remediation - CR is not created, status updated correctly", func() {
					cr := newRemediationCR("unhealthy-worker-node-1", underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					Expect(*underTest.Status.HealthyNodes).To(Equal(3))
					Expect(*underTest.Status.ObservedNodes).To(Equal(7))
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
				})

			})

			When("few nodes become healthy", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
					remediationCR := newRemediationCR("healthy-worker-node-2", underTest)
					remediationCROther := newRemediationCR("healthy-worker-node-1", underTest)
					refs := remediationCROther.GetOwnerReferences()
					refs[0].Name = "other"
					remediationCROther.SetOwnerReferences(refs)
					objects = append(objects, remediationCR, remediationCROther)
				})

				It("deletes an existing remediation CR and updates status", func() {
					cr := newRemediationCR("healthy-worker-node-2", underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					// owned by other NHC, should not be deleted
					cr = newRemediationCR("healthy-worker-node-1", underTest)
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())

					cr = newRemediationCR("unhealthy-worker-node-1", underTest)
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())

					Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
					Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
				})
			})

			When("an old remediation cr exists", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
				})

				AfterEach(func() {
					fakeTime = nil
				})

				It("an alert flag is set on remediation cr", func() {
					By("faking time and triggering another reconcile")
					afterTimeout := time.Now().Add(remediationCRAlertTimeout).Add(2 * time.Minute)
					fakeTime = &afterTimeout
					labels := underTest.Labels
					if labels == nil {
						labels = make(map[string]string)
					}
					labels["trigger"] = "now"
					underTest.Labels = labels
					Expect(k8sClient.Update(context.Background(), underTest)).To(Succeed())
					time.Sleep(2 * time.Second)

					cr := newRemediationCR("unhealthy-worker-node-1", underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())
					Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(Equal("flagon"))
				})
			})

			When("a remediation cr not owned by current NHC exists", func() {
				BeforeEach(func() {
					cr := newRemediationCR("unhealthy-worker-node-1", underTest)
					owners := cr.GetOwnerReferences()
					owners[0].Name = "not-me"
					cr.SetOwnerReferences(owners)
					Expect(k8sClient.Create(context.Background(), cr)).To(Succeed())
					setupObjects(1, 2, true)
				})

				It("remediation cr should not be processed", func() {
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				})
			})
		}

		Context("with spec.remediationTemplate", func() {
			testReconcile()
		})

		Context("with a single escalating remediation", func() {

			BeforeEach(func() {
				templateRef := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil
				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef,
						Order:               0,
						Timeout:             metav1.Duration{Duration: time.Minute},
					},
				}
			})

			testReconcile()
		})

		Context("with multiple escalating remediations", func() {

			BeforeEach(func() {
				templateRef1 := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil

				templateRef2 := templateRef1.DeepCopy()
				templateRef2.Kind = "Metal3RemediationTemplate"
				templateRef2.Name = "ok"
				templateRef2.Namespace = MachineNamespace

				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef1,
						Order:               0,
						Timeout:             metav1.Duration{Duration: 5 * time.Second},
					},
					{
						RemediationTemplate: *templateRef2,
						Order:               5,
						Timeout:             metav1.Duration{Duration: 15 * time.Second},
					},
				}

				setupObjects(1, 2, false)

			})

			It("it should try one remediation after another", func() {
				cr := newRemediationCR("unhealthy-worker-node-1", underTest)
				// first call should fail, because the node gets unready in a few seconds only
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())
				// wait until nodes are unhealthy
				time.Sleep(nodeUnhealthyIn)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				// Wait for 1st remediation to time out and 2nd to start
				time.Sleep(7 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				// get new CR
				cr = newRemediationCRForSecondRemediation("unhealthy-worker-node-1", underTest)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(2))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Name).To(Equal(cr.GetName()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Namespace).To(Equal(cr.GetNamespace()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.UID).To(Equal(cr.GetUID()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).To(BeNil())

				// Wait for 2nd remediation to time out
				time.Sleep(17 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				// make node healthy
				node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unhealthy-worker-node-1"}}
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Status.Conditions[0].Status = v1.ConditionTrue
				Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

				// wait a bit
				time.Sleep(2 * time.Second)

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(3))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(0))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))

			})
		})

		Context("with progressing condition being set", func() {

			BeforeEach(func() {
				templateRef1 := underTest.Spec.RemediationTemplate
				underTest.Spec.RemediationTemplate = nil
				underTest.Spec.EscalatingRemediations = []v1alpha1.EscalatingRemediation{
					{
						RemediationTemplate: *templateRef1,
						Order:               0,
						Timeout:             metav1.Duration{Duration: 5 * time.Minute},
					},
				}
				setupObjects(1, 2, true)
			})

			It("it should timeout early", func() {
				cr := newRemediationCR("unhealthy-worker-node-1", underTest)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())

				By("letting the remediation stop progressing")
				conditions := []interface{}{
					map[string]interface{}{
						"type":               "Succeeded",
						"status":             "False",
						"lastTransitionTime": time.Now().Format(time.RFC3339),
					},
				}
				unstructured.SetNestedSlice(cr.Object, conditions, "status", "conditions")
				Expect(k8sClient.Status().Update(context.Background(), cr))

				// Wait for hardcoded timeout to expire
				time.Sleep(5 * time.Second)

				// get updated CR
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).ToNot(BeNil())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
			})
		})

		Context("control plane nodes", func() {
			When("two control plane nodes are unhealthy, they should be remediated one after another", func() {
				BeforeEach(func() {
					objects = newNodes(2, 1, true, true)
					objects = append(objects, newNodes(1, 5, false, true)...)
					underTest = newNodeHealthCheck()
					objects = append(objects, underTest)
				})

				It("creates a one remediation CR for control plane node and updates status", func() {
					cr := newRemediationCR("", underTest)
					crList := &unstructured.UnstructuredList{Object: cr.Object}
					Expect(k8sClient.List(context.Background(), crList)).To(Succeed())

					Expect(len(crList.Items)).To(BeNumerically("==", 2), "expected 2 remediations, one for control plane, one for worker")
					Expect(crList.Items).To(ContainElements(
						// the unhealthy worker
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", "unhealthy-worker-node-1"))),
						// one of the unhealthy control plane nodes
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", ContainSubstring("unhealthy-control-plane-node")))),
					))
					Expect(*underTest.Status.HealthyNodes).To(Equal(6))
					Expect(*underTest.Status.ObservedNodes).To(Equal(9))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(2))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(2))
					Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
						And(
							HaveField("Name", "unhealthy-worker-node-1"),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", "unhealthy-worker-node-1"),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
						And(
							HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", ContainSubstring("unhealthy-control-plane-node")),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
					))

					var unhealthyCPNodeName string
					for _, unhealthyNode := range underTest.Status.UnhealthyNodes {
						if strings.Contains(unhealthyNode.Name, "unhealthy-control-plane-node") {
							unhealthyCPNodeName = unhealthyNode.Name
							break
						}
					}
					Expect(unhealthyCPNodeName).ToNot(BeEmpty())

					By("simulating remediator by putting a finalizer on the cp remediation CR")
					for _, cr := range crList.Items {
						if cr.GetName() == unhealthyCPNodeName {
							cr.SetFinalizers([]string{"dummy"})
							Expect(k8sClient.Update(context.Background(), &cr)).To(Succeed())
							break
						}
					}

					By("make cp node healthy")
					unhealthyCPNode := &v1.Node{}
					unhealthyCPNode.Name = unhealthyCPNodeName
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyCPNode), unhealthyCPNode)).To(Succeed())
					unhealthyCPNode.Status.Conditions = []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					}
					Expect(k8sClient.Status().Update(context.Background(), unhealthyCPNode))

					By("waiting for remediation end of cp node")
					Eventually(func() []*v1alpha1.UnhealthyNode {
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						return underTest.Status.UnhealthyNodes

					}, "2s", "100ms").Should(HaveLen(1))

					By("ensuring other cp node isn't remediated yet")
					Consistently(func() []*v1alpha1.UnhealthyNode {
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						return underTest.Status.UnhealthyNodes

					}, "5s", "1s").Should(HaveLen(1))

					By("simulating remediator finished by removing finalizer on the cp remediation CR")
					for _, cr := range crList.Items {
						if cr.GetName() == unhealthyCPNodeName {
							Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&cr), &cr)).To(Succeed())
							cr.SetFinalizers([]string{})
							Expect(k8sClient.Update(context.Background(), &cr)).To(Succeed())
							break
						}
					}

					By("ensuring other cp node is remediated now")
					Eventually(func() []*v1alpha1.UnhealthyNode {
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						return underTest.Status.UnhealthyNodes
					}, "2s", "100ms").Should(ContainElements(
						And(
							HaveField("Name", "unhealthy-worker-node-1"),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", "unhealthy-worker-node-1"),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
						And(
							HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
							// ensure it's the other cp node now
							Not(HaveField("Name", unhealthyCPNodeName)),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", ContainSubstring("unhealthy-control-plane-node")),
									HaveField("Started", Not(BeNil())),
									HaveField("TimedOut", BeNil()),
								),
							)),
						),
					))
					crList = &unstructured.UnstructuredList{Object: cr.Object}
					Expect(k8sClient.List(context.Background(), crList)).To(Succeed())
					Expect(len(crList.Items)).To(BeNumerically("==", 2), "expected 2 remediations, one for control plane, one for worker")
					Expect(crList.Items).To(ContainElements(
						// the unhealthy worker
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", "unhealthy-worker-node-1"))),
						// the other unhealthy control plane nodes
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", ContainSubstring("unhealthy-control-plane-node")))),
					))
					Expect(crList.Items).ToNot(ContainElements(
						// the old unhealthy control plane node
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", unhealthyCPNodeName))),
					))

				})
			})
		})

		When("remediation is needed but pauseRequests exists", func() {
			BeforeEach(func() {
				setupObjects(1, 2, true)
				underTest.Spec.PauseRequests = []string{"I'm an admin, asking you to stop remediating this group of nodes"}
			})

			It("skips remediation and updates status", func() {
				cr := newRemediationCR("unhealthy-worker-node-1", underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhasePaused))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())
			})
		})

		When("Nodes are candidates for remediation and cluster is upgrading", func() {
			BeforeEach(func() {
				clusterUpgradeRequeueAfter = 5 * time.Second
				upgradeChecker.Upgrading = true
				setupObjects(1, 2, true)
			})

			AfterEach(func() {
				upgradeChecker.Upgrading = false
			})

			It("doesn't not remediate but requeues reconciliation and updates status", func() {
				cr := newRemediationCR("unhealthy-worker-node-1", underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
				Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
				Expect(underTest.Status.Reason).ToNot(BeEmpty())

				By("stopping upgrade and waiting for requeue")
				upgradeChecker.Upgrading = false
				time.Sleep(10 * time.Second)
				err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(err).ToNot(HaveOccurred())

				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(2))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
			})

		})

		Context("Machine owners", func() {
			When("Metal3RemediationTemplate is in correct namespace", func() {

				var machine *machinev1beta1.Machine

				BeforeEach(func() {
					setupObjects(1, 2, true)

					// create machine
					machine = &machinev1beta1.Machine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-machine",
							Namespace: MachineNamespace,
						},
					}
					objects = append(objects, machine)

					// set machine annotation to unhealthy node
					for _, o := range objects {
						o := o
						if o.GetName() == "unhealthy-worker-node-1" {
							ann := make(map[string]string)
							ann["machine.openshift.io/machine"] = fmt.Sprintf("%s/%s", machine.Namespace, machine.Name)
							o.SetAnnotations(ann)
						}
					}

					// set metal3 template
					underTest.Spec.RemediationTemplate.Kind = "Metal3RemediationTemplate"
					underTest.Spec.RemediationTemplate.Name = "ok"
					underTest.Spec.RemediationTemplate.Namespace = MachineNamespace

				})

				It("should set owner ref to the machine", func() {
					cr := newRemediationCR("unhealthy-worker-node-1", underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.GetOwnerReferences()).To(
						ContainElement(
							And(
								// Kind and API version aren't set on underTest, envtest issue...
								// Controller is empty for HaveField because false is the zero value?
								HaveField("Name", machine.Name),
								HaveField("UID", machine.UID),
							),
						),
					)
				})
			})

		})

	})

	// TODO move to new suite in utils package
	Context("Controller Watches", func() {
		var (
			underTest1 *v1alpha1.NodeHealthCheck
			underTest2 *v1alpha1.NodeHealthCheck
			objects    []client.Object
		)

		JustBeforeEach(func() {
			createObjects(objects...)
			time.Sleep(2 * time.Second)
		})

		AfterEach(func() {
			deleteObjects(objects...)
			time.Sleep(1 * time.Second)
		})

		When("a node changes status and is selectable by one NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				emptySelector, _ := metav1.ParseToLabelSelector("fooLabel=bar")
				underTest2.Spec.Selector = *emptySelector
				objects = append(objects, underTest1, underTest2)
			})

			It("creates a reconcile request", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(1))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
			})
		})

		When("a node changes status and is selectable by the more 2 NHC selector", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
				underTest1 = newNodeHealthCheck()
				underTest2 = newNodeHealthCheck()
				underTest2.Name = "test-2"
				objects = append(objects, underTest1, underTest2)
			})

			It("creates 2 reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(len(requests)).To(Equal(2))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest1.GetName()}}))
				Expect(requests).To(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: underTest2.GetName()}}))
			})
		})
		When("a node changes status and there are no NHC objects", func() {
			BeforeEach(func() {
				objects = newNodes(3, 10, false, true)
			})

			It("doesn't create reconcile requests", func() {
				handler := utils.NHCByNodeMapperFunc(k8sClient, controllerruntime.Log)
				updatedNode := v1.Node{
					ObjectMeta: controllerruntime.ObjectMeta{Name: "healthy-worker-node-1"},
				}
				requests := handler(&updatedNode)
				Expect(requests).To(BeEmpty())
			})
		})
	})

	Context("Node updates", func() {
		var oldConditions []v1.NodeCondition
		var newConditions []v1.NodeCondition

		When("no Ready condition exists on new node", func() {
			BeforeEach(func() {
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should not request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeFalse())
			})
		})

		When("condition types and statuses equal", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should not request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeFalse())
			})
		})

		When("condition type changed", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition status changed", func() {
			BeforeEach(func() {
				oldConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				}
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionFalse,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition was added", func() {
			BeforeEach(func() {
				oldConditions = append(newConditions,
					v1.NodeCondition{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
				)
				newConditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionFalse,
					},
				}
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})

		When("condition was removed", func() {
			BeforeEach(func() {
				oldConditions = append(newConditions,
					v1.NodeCondition{
						Type:   v1.NodeReady,
						Status: v1.ConditionTrue,
					},
					v1.NodeCondition{
						Type:   v1.NodeDiskPressure,
						Status: v1.ConditionTrue,
					},
				)

				newConditions = append(newConditions, v1.NodeCondition{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				})
			})
			It("should request reconcile", func() {
				Expect(conditionsNeedReconcile(oldConditions, newConditions)).To(BeTrue())
			})
		})
	})
})

func newRemediationCR(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	return newRemediationCRImpl(nodeName, nhc, false)
}

func newRemediationCRForSecondRemediation(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	return newRemediationCRImpl(nodeName, nhc, true)
}

func newRemediationCRImpl(nodeName string, nhc *v1alpha1.NodeHealthCheck, use2ndEscRem bool) *unstructured.Unstructured {

	var templateRef v1.ObjectReference
	if nhc.Spec.RemediationTemplate != nil {
		templateRef = *nhc.Spec.RemediationTemplate
	} else {
		templateRef = nhc.Spec.EscalatingRemediations[0].RemediationTemplate
		if use2ndEscRem {
			templateRef = nhc.Spec.EscalatingRemediations[1].RemediationTemplate
		}
	}

	cr := unstructured.Unstructured{}
	cr.SetName(nodeName)
	cr.SetNamespace(templateRef.Namespace)
	kind := templateRef.GroupVersionKind().Kind
	// remove trailing template
	kind = kind[:len(kind)-len("template")]
	cr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   templateRef.GroupVersionKind().Group,
		Version: templateRef.GroupVersionKind().Version,
		Kind:    kind,
	})
	cr.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: nhc.APIVersion,
			Kind:       nhc.Kind,
			Name:       nhc.Name,
			UID:        nhc.UID,
		},
	})
	return &cr
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
			UID:  "1234",
		},
		Spec: v1alpha1.NodeHealthCheckSpec{
			Selector:   metav1.LabelSelector{},
			MinHealthy: &unhealthy,
			UnhealthyConditions: []v1alpha1.UnhealthyCondition{
				{
					Type:     v1.NodeReady,
					Status:   v1.ConditionFalse,
					Duration: metav1.Duration{Duration: unhealthyConditionDuration},
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

func newNodes(unhealthy int, healthy int, isControlPlane bool, unhealthyNow bool) []client.Object {
	o := make([]client.Object, 0, healthy+unhealthy)
	roleName := "-worker"
	if isControlPlane {
		roleName = "-control-plane"
	}
	for i := unhealthy; i > 0; i-- {
		node := newNode(fmt.Sprintf("unhealthy%s-node-%d", roleName, i), v1.NodeReady, v1.ConditionFalse, isControlPlane, unhealthyNow)
		o = append(o, node)
	}
	for i := healthy; i > 0; i-- {
		o = append(o, newNode(fmt.Sprintf("healthy%s-node-%d", roleName, i), v1.NodeReady, v1.ConditionTrue, isControlPlane, unhealthyNow))
	}
	return o
}

func newNode(name string, t v1.NodeConditionType, s v1.ConditionStatus, isControlPlane bool, unhealthyNow bool) client.Object {
	labels := make(map[string]string, 1)
	if isControlPlane {
		labels[utils.ControlPlaneRoleLabel] = ""
	} else {
		labels[utils.WorkerRoleLabel] = ""
	}
	// let the node get unhealthy in a few seconds
	transitionTime := time.Now().Add(-(unhealthyConditionDuration - nodeUnhealthyIn + 2*time.Second))
	// unless requested otherwise
	if unhealthyNow {
		transitionTime = time.Now().Add(-(unhealthyConditionDuration + 2*time.Second))
	}
	return &v1.Node{
		TypeMeta: metav1.TypeMeta{Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:               t,
					Status:             s,
					LastTransitionTime: metav1.Time{Time: transitionTime},
				},
			},
		},
	}
}
