package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	commonannotations "github.com/medik8s/common/pkg/annotations"
	commonconditions "github.com/medik8s/common/pkg/conditions"
	commonLabels "github.com/medik8s/common/pkg/labels"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"

	"github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/resources"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils/annotations"
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
					Selector:            metav1.LabelSelector{},
					RemediationTemplate: infraRemediationTemplateRef.DeepCopy(),
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
					RemediationTemplate: infraRemediationTemplateRef.DeepCopy(),
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
		const (
			unhealthyNodeName = "unhealthy-worker-node-1"
		)
		var (
			underTest *v1alpha1.NodeHealthCheck
			objects   []client.Object
			//Lease params
			leaseName                             = fmt.Sprintf("%s-%s", "node", unhealthyNodeName)
			mockRequeueDurationIfLeaseTaken       = time.Second * 2
			mockDefaultLeaseDuration              = time.Second * 2
			mockLeaseBuffer                       = time.Second
			otherLeaseDurationInSeconds     int32 = 3
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
				cr := newRemediationCRForNHC("", underTest)
				crList := &unstructured.UnstructuredList{Object: cr.Object}
				Expect(k8sClient.List(context.Background(), crList)).To(Succeed())
				for _, item := range crList.Items {
					Expect(k8sClient.Delete(context.Background(), &item)).To(Succeed())
				}
			}

			//cleanup lease
			lease := &coordv1.Lease{}
			err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: leaseNs, Name: leaseName}, lease)
			if err == nil {
				err = k8sClient.Delete(context.Background(), lease)
				Expect(err).NotTo(HaveOccurred())
			}

			// let thing settle a bit
			time.Sleep(1 * time.Second)
		})

		testReconcile := func() {

			When("Remediation template is broken", func() {

				BeforeEach(func() {
					setupObjects(0, 2, true)
				})

				expectTemplateNotFound := func(g Gomega, nhc *v1alpha1.NodeHealthCheck, expectedError string) {
					g.ExpectWithOffset(1, underTest.Status.Phase).To(Equal(v1alpha1.PhaseDisabled))
					g.ExpectWithOffset(1, underTest.Status.Reason).To(ContainSubstring(expectedError))
					g.ExpectWithOffset(1, underTest.Status.Conditions).To(ContainElement(
						And(
							HaveField("Type", v1alpha1.ConditionTypeDisabled),
							HaveField("Status", metav1.ConditionTrue),
							HaveField("Reason", v1alpha1.ConditionReasonDisabledTemplateNotFound),
						)))
				}

				Context("with invalid kind", func() {
					BeforeEach(func() {
						if underTest.Spec.RemediationTemplate != nil {
							underTest.Spec.RemediationTemplate.Kind = "dummyTemplate"
						} else {
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Kind = "dummyTemplate"
						}
					})

					It("should set corresponding condition", func() {
						expectTemplateNotFound(Default, underTest, "failed to get")
					})
				})

				Context("with missing namespace", func() {
					BeforeEach(func() {
						if underTest.Spec.RemediationTemplate != nil {
							underTest.Spec.RemediationTemplate.Namespace = ""
						} else {
							underTest.Spec.EscalatingRemediations[0].RemediationTemplate.Namespace = ""
						}
					})

					It("should set corresponding condition", func() {
						expectTemplateNotFound(Default, underTest, "no namespace is provided")
					})
				})

				Context("templated is deleted after NHC creation", func() {
					It("should set corresponding condition", func() {
						By("deleting template")
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(infraRemediationTemplate), infraRemediationTemplate)).To(Succeed())
						Expect(k8sClient.Delete(context.Background(), infraRemediationTemplate)).To(Succeed())
						DeferCleanup(func() {
							By("recreating template")
							infraRemediationTemplate.SetResourceVersion("")
							Expect(k8sClient.Create(context.Background(), infraRemediationTemplate)).To(Succeed())
						})
						Eventually(func(g Gomega) {
							g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
							expectTemplateNotFound(g, underTest, "failed to get")
						}, "5s", "200ms").Should(Succeed(), "expected disabled NHC")
					})
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

					By("checking CR isn't created when unhealthy duration didn't expire yet")

					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					// first call should fail, because the node gets unready in a few seconds only
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					By("waiting until nodes are unhealthy")
					time.Sleep(nodeUnhealthyIn)

					By("checking CR is created now")
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

					By("simulating remediator by putting a finalizer on the remediation CR")
					cr.SetFinalizers([]string{"dummy"})
					Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())

					By("checking NHC status")
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

					By("making node ready")
					unhealthyNode := &v1.Node{}
					unhealthyNode.Name = unhealthyNodeName
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyNode), unhealthyNode)).To(Succeed())
					unhealthyNode.Status.Conditions = []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					}
					Expect(k8sClient.Status().Update(context.Background(), unhealthyNode))

					By("expecting status update")
					Eventually(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						g.Expect(underTest.Status.UnhealthyNodes[0].ConditionsHealthyTimestamp).ToNot(BeNil())
						// ensure node is still considered unhealthy though
						g.Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						g.Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
						g.Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
					}, "5s", "500ms").Should(Succeed(), "expected conditionsHealthyTimestamp to be set")

					By("simulating remediator finished by removing finalizer")
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					cr.SetFinalizers([]string{})
					Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())

					By("expecting CR deletion")
					Eventually(func(g Gomega) {
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(errors.IsNotFound(err)).To(BeTrue())
					}, "5s", "500ms").Should(Succeed(), "expected CR deletion")

					By("expecting status update")
					Eventually(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						g.Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
						g.Expect(underTest.Status.UnhealthyNodes).To(HaveLen(0))
						g.Expect(*underTest.Status.HealthyNodes).To(Equal(3))
						g.Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
					}, "5s", "500ms").Should(Succeed(), "expected conditionsHealthyTimestamp to be set")

				})

			})

			When("few nodes are unhealthy and healthy nodes below min healthy", func() {
				BeforeEach(func() {
					setupObjects(4, 3, true)
				})

				It("skips remediation - CR is not created, status updated correctly", func() {
					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(errors.IsNotFound(err)).To(BeTrue())

					Expect(*underTest.Status.HealthyNodes).To(Equal(3))
					Expect(*underTest.Status.ObservedNodes).To(Equal(7))
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(4))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))
					Expect(underTest.Status.UnhealthyNodes[1].Remediations).To(HaveLen(0))
					Expect(underTest.Status.UnhealthyNodes[2].Remediations).To(HaveLen(0))
					Expect(underTest.Status.UnhealthyNodes[3].Remediations).To(HaveLen(0))
					Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
					Expect(underTest.Status.Reason).ToNot(BeEmpty())
				})

			})

			When("few nodes become healthy", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
					remediationCR := newRemediationCRForNHC("healthy-worker-node-2", underTest)
					remediationCR.SetFinalizers([]string{"dummy"})
					remediationCROther := newRemediationCRForNHC("healthy-worker-node-1", underTest)
					refs := remediationCROther.GetOwnerReferences()
					refs[0].Name = "other"
					remediationCROther.SetOwnerReferences(refs)
					objects = append(objects, remediationCR, remediationCROther)
				})

				It("deletes an existing remediation CR and updates status", func() {
					By("verifying CR owned by other NHC isn't deleted")
					cr := newRemediationCRForNHC("healthy-worker-node-1", underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())

					By("verifying CR owned by us has deletion timestamp")
					cr = newRemediationCRForNHC("healthy-worker-node-2", underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.GetDeletionTimestamp()).ToNot(BeNil())

					By("verifying node with CR is still considered unhealthy")
					Expect(*underTest.Status.HealthyNodes).To(Equal(1))
					Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					// don't test Status.InFlightRemediations / Status.UnhealthyNodes here, they aren't updated for
					// the existing CR created by the test...

					By("simulating remediator finished by removing finalizer on the cp remediation CR")
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					cr.SetFinalizers([]string{})
					Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())

					By("verifying CR is deleted")
					Eventually(func(g Gomega) {
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						g.Expect(errors.IsNotFound(err)).To(BeTrue())
					}, "2s", "100s").Should(Succeed())

					By("verifying next node remediated now")
					cr = newRemediationCRForNHC(unhealthyNodeName, underTest)
					Eventually(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					}, "2s", "100ms").Should(Succeed())

					By("verifying status")
					Eventually(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						g.Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					}, "2s", "100ms").Should(Succeed())
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
					newMinHealthy := intstr.FromString("52%")
					underTest.Spec.MinHealthy = &newMinHealthy
					Expect(k8sClient.Update(context.Background(), underTest)).To(Succeed())
					time.Sleep(2 * time.Second)

					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					Expect(err).NotTo(HaveOccurred())
					Expect(cr.GetAnnotations()[oldRemediationCRAnnotationKey]).To(Equal("flagon"))
				})
			})

			When("a remediation cr not owned by current NHC exists", func() {
				BeforeEach(func() {
					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					owners := cr.GetOwnerReferences()
					owners[0].Name = "not-me"
					cr.SetOwnerReferences(owners)
					Expect(k8sClient.Create(context.Background(), cr)).To(Succeed())
					setupObjects(1, 2, true)
				})

				It("remediation cr should not be processed", func() {
					Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(unhealthyNodeName))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))
				})
			})

			When("two NHC CRs with different templates target the same unhealthy node", func() {

				otherTestCRName := "other-test"
				var otherTestCR *v1alpha1.NodeHealthCheck

				BeforeEach(func() {
					// prepare other CR but do not create yet, in order to have a predictable order of things to happen
					otherTestCR = underTest.DeepCopy()
					otherTestCR.SetName(otherTestCRName)
					otherTestCR.Spec.RemediationTemplate = &v1.ObjectReference{
						Kind:       "Metal3RemediationTemplate",
						Namespace:  MachineNamespace,
						Name:       "ok",
						APIVersion: "test.medik8s.io/v1alpha1",
					}

					setupObjects(1, 2, true)
				})

				It("only one NHC should remediate that node", func() {

					By("Verifying node is remediated by 1st NHC")
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))

					By("Creating a 2nd NHC")
					Expect(k8sClient.Create(context.Background(), otherTestCR)).To(Succeed())
					DeferCleanup(func() {
						Expect(k8sClient.Delete(context.Background(), otherTestCR)).To(Succeed())
					})
					By("Verifying node is NOT remediated by 2nd NHC")
					// wait for unhealthy node
					Eventually(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(otherTestCR), otherTestCR)).To(Succeed())
						g.Expect(otherTestCR.Status.UnhealthyNodes).To(HaveLen(1))
					}, "5s", "1s").Should(Succeed(), "unhealthy node not detected")
					// ensure no remediation starts
					Consistently(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(otherTestCR), otherTestCR)).To(Succeed())
						g.Expect(otherTestCR.Status.InFlightRemediations).To(HaveLen(0))
						g.Expect(otherTestCR.Status.UnhealthyNodes).To(HaveLen(1))
						g.Expect(otherTestCR.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))
					}, "5s", "1s").Should(Succeed(), "duplicate remediation detected!")
				})
			})

		}

		Context("with spec.remediationTemplate", func() {
			testReconcile()

			Context("Node Lease", func() {

				BeforeEach(func() {
					setupObjects(1, 2, true)
				})
				When("an unhealthy node becomes healthy", func() {
					It("node lease is removed", func() {
						cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(err).ToNot(HaveOccurred())
						//Verify lease exist
						lease := &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(err).ToNot(HaveOccurred())

						//Mock node becoming healthy
						node := &v1.Node{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)
						Expect(err).ToNot(HaveOccurred())
						for i, c := range node.Status.Conditions {
							if c.Type == v1.NodeReady {
								node.Status.Conditions[i].Status = v1.ConditionTrue
							}
						}
						err = k8sClient.Status().Update(context.Background(), node)
						Expect(err).ToNot(HaveOccurred())

						//Remediation should be removed
						Eventually(func() bool {
							err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
							return errors.IsNotFound(err)
						}, "2s", "100ms").Should(BeTrue(), "remediation CR wasn't removed")

						//Verify NHC removed the lease
						Eventually(func() bool {
							err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
							return errors.IsNotFound(err)
						}, "2s", "100ms").Should(BeTrue(), "lease wasn't removed")

					})

					It("node lease not owned by us isn't removed, but status is updated (invalidate lease error is ignored)", func() {
						cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(err).ToNot(HaveOccurred())
						//Verify lease exist
						lease := &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(err).ToNot(HaveOccurred())

						// change lease owner
						newLeaseOwner := "someone-else"
						lease.Spec.HolderIdentity = pointer.String(newLeaseOwner)
						Expect(k8sClient.Update(context.Background(), lease)).To(Succeed(), "failed to update lease owner")

						//Mock node becoming healthy
						node := &v1.Node{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)
						Expect(err).ToNot(HaveOccurred())
						for i, c := range node.Status.Conditions {
							if c.Type == v1.NodeReady {
								node.Status.Conditions[i].Status = v1.ConditionTrue
							}
						}
						err = k8sClient.Status().Update(context.Background(), node)
						Expect(err).ToNot(HaveOccurred())

						//Remediation should be removed
						Eventually(func() bool {
							err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
							return errors.IsNotFound(err)
						}, "2s", "100ms").Should(BeTrue(), "remediation CR wasn't removed")

						// Status should be updated even though lease isn't owned by us anymore
						Eventually(func(g Gomega) {
							g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
							g.Expect(underTest.Status.InFlightRemediations).To(BeEmpty())
							g.Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
						}, "2s", "100ms").Should(Succeed(), "status update failed")

						//Verify NHC didn't touch the lease
						Consistently(func(g Gomega) {
							g.Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)).To(Succeed(), "failed to get lease")
							g.Expect(*lease.Spec.HolderIdentity).To(Equal(newLeaseOwner))
						}, "2s", "100ms").Should(Succeed(), "lease was touched even though it's not owned by us")

					})
				})

				When("an unhealthy node lease is already taken", func() {
					BeforeEach(func() {
						mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer)

						//Create a mock lease that is already taken
						now := metav1.NowMicro()
						lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: leaseNs}, Spec: coordv1.LeaseSpec{HolderIdentity: pointer.String("notNHC"), LeaseDurationSeconds: &otherLeaseDurationInSeconds, RenewTime: &now, AcquireTime: &now}}
						err := k8sClient.Create(context.Background(), lease)
						Expect(err).NotTo(HaveOccurred())
					})

					It("a remediation CR isn't created until lease is obtained", func() {
						cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(errors.IsNotFound(err)).To(BeTrue())

						Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						Expect(*underTest.Status.ObservedNodes).To(Equal(3))
						Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
						Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
						Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))

						Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))
						Expect(underTest.Status.Reason).ToNot(BeEmpty())
						Expect(underTest.Status.Conditions).To(ContainElement(
							And(
								HaveField("Type", v1alpha1.ConditionTypeDisabled),
								HaveField("Status", metav1.ConditionFalse),
								HaveField("Reason", v1alpha1.ConditionReasonEnabled),
							)))
						//expecting NHC to acquire the lease now and create the CR - checking CR first
						Eventually(
							func() error {
								return k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
							},
							mockRequeueDurationIfLeaseTaken*2+time.Millisecond*100, time.Millisecond*100).ShouldNot(HaveOccurred())

						//Verifying lease is created
						lease := &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						Expect(err).ToNot(HaveOccurred())
						Expect(*lease.Spec.HolderIdentity).To(Equal(fmt.Sprintf("%s-%s", "NodeHealthCheck", underTest.GetName())))
						Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(2 + 1 /*2 seconds is DefaultLeaseDuration (mocked) + 1 second buffer (mocked)  */)))
						Expect(lease.Spec.AcquireTime).ToNot(BeNil())
						Expect(*lease.Spec.AcquireTime).To(Equal(*lease.Spec.RenewTime))

						leaseExpireTime := lease.Spec.AcquireTime.Time.Add(mockRequeueDurationIfLeaseTaken*3 + mockLeaseBuffer)
						timeLeftForLease := leaseExpireTime.Sub(time.Now())
						//Wait for lease to be extended
						time.Sleep(timeLeftForLease * 3 / 4)
						lease = &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						//Verify NHC extended the lease
						Expect(err).ToNot(HaveOccurred())
						Expect(*lease.Spec.AcquireTime).ToNot(Equal(*lease.Spec.RenewTime))
						Expect(lease.Spec.RenewTime.Sub(lease.Spec.AcquireTime.Time) > 0).To(BeTrue())

						//Wait for lease to expire
						time.Sleep(timeLeftForLease/4 + time.Millisecond*100)
						lease = &coordv1.Lease{}
						err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
						//Verify NHC removed the lease
						Expect(errors.IsNotFound(err)).To(BeTrue())
						//Verify NHC sets timeout annotation
						err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						Expect(err).ToNot(HaveOccurred())
						_, isNhcTimeOutSet := cr.GetAnnotations()[commonannotations.NhcTimedOut]
						Expect(isNhcTimeOutSet).To(BeTrue())

					})

				})
			})

			When("unhealthy condition changes", func() {
				BeforeEach(func() {
					setupObjects(1, 2, true)
				})

				It("should not delete CR when duration didn't expire yet", func() {
					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

					By("changing to other unhealthy condition")
					node := &v1.Node{}
					Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)).To(Succeed())
					Expect(node.Status.Conditions[0].Status).ToNot(Equal(v1.ConditionFalse))
					node.Status.Conditions[0].Status = v1.ConditionFalse
					node.Status.Conditions[0].LastTransitionTime = metav1.Now()
					Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

					By("ensuring CR isn't deleted")
					Consistently(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					}, unhealthyConditionDuration*2, "1s").Should(Succeed(), "CR was deleted")

					By("changing to healthy condition")
					Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)).To(Succeed())
					node.Status.Conditions[0].Status = v1.ConditionTrue
					node.Status.Conditions[0].LastTransitionTime = metav1.Now()
					Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

					By("ensuring CR is deleted")
					Eventually(func(g Gomega) {
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						g.Expect(errors.IsNotFound(err)).To(BeTrue())
					}, "2s", "200ms").Should(Succeed(), "CR wasn't deleted")
				})

			})

		})

		Context("with Node marked for excluding remediation", func() {
			BeforeEach(func() {
				objects = newNodes(1, 2, false, true)
				objects = append(objects, newNodeHealthCheck())
				node := objects[0].(*v1.Node)
				node.GetLabels()[commonLabels.ExcludeFromRemediation] = "true"

			})
			It("remediation shouldn't be created", func() {
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(0))
			})
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
			firstRemediationTimeout := time.Second
			secondRemediationTimeout := 4 * time.Second
			thirdRemediationTimeout := time.Second
			forthRemediationTimeout := time.Second
			BeforeEach(func() {
				mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer)

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
						Timeout:             metav1.Duration{Duration: firstRemediationTimeout},
					},
					{
						RemediationTemplate: *templateRef2,
						Order:               5,
						Timeout:             metav1.Duration{Duration: secondRemediationTimeout},
					},
					{
						RemediationTemplate: *multiSupportTemplateRef,
						Order:               6,
						Timeout:             metav1.Duration{Duration: thirdRemediationTimeout},
					},
					{
						RemediationTemplate: *secondMultiSupportTemplateRef,
						Order:               8,
						Timeout:             metav1.Duration{Duration: forthRemediationTimeout},
					},
				}

				setupObjects(1, 2, false)

			})

			It("it should try one remediation after another", func() {
				cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
				// first call should fail, because the node gets unready in a few seconds only
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				// wait until nodes are unhealthy
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					g.Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					g.Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					g.Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					g.Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					g.Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(1))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Name).To(Equal(cr.GetName()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.Namespace).To(Equal(cr.GetNamespace()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.UID).To(Equal(cr.GetUID()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Started).ToNot(BeNil())
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).To(BeNil())
					g.Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				//Verify lease is created
				lease := &coordv1.Lease{}
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(err).ToNot(HaveOccurred())
				Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(firstRemediationTimeout.Seconds() + mockLeaseBuffer.Seconds()) /*First escalation timeout (1) + buffer (1) */))
				Expect(lease.Spec.AcquireTime).ToNot(BeNil())
				Expect(*lease.Spec.AcquireTime).To(Equal(*lease.Spec.RenewTime))

				// Wait for 1st remediation to time out and 2nd to start
				Eventually(func(g Gomega) {
					// get updated CR
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					g.Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))

				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// get new CR
				newCr := newRemediationCRForNHCSecondRemediation(unhealthyNodeName, underTest)
				Eventually(func() error {
					return k8sClient.Get(context.Background(), client.ObjectKeyFromObject(newCr), newCr)
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// get updated NHC
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].Resource.GroupVersionKind()).To(Equal(cr.GroupVersionKind()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[0].TimedOut).ToNot(BeNil())
					g.Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

					g.Expect(*underTest.Status.HealthyNodes).To(Equal(2))
					g.Expect(*underTest.Status.ObservedNodes).To(Equal(3))
					g.Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
					g.Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
					g.Expect(underTest.Status.UnhealthyNodes[0].Name).To(Equal(cr.GetName()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations).To(HaveLen(2))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(newCr.GroupVersionKind()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Name).To(Equal(newCr.GetName()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.Namespace).To(Equal(newCr.GetNamespace()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.UID).To(Equal(newCr.GetUID()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Started).ToNot(BeNil())
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).To(BeNil())

				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				//Verify lease was extended
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(err).ToNot(HaveOccurred())
				Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(secondRemediationTimeout.Seconds() + mockLeaseBuffer.Seconds())))
				Expect(lease.Spec.AcquireTime).ToNot(BeNil())
				Expect(lease.Spec.RenewTime.Sub(lease.Spec.AcquireTime.Time) > 0).To(BeTrue())

				// Wait for 2nd remediation to time out
				Eventually(func(g Gomega) {
					// get updated CR
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(newCr), newCr)).To(Succeed())
					g.Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))
					g.Expect(newCr.GetName()).To(Equal(unhealthyNodeName))
					g.Expect(newCr.GetAnnotations()).ToNot(HaveKey(Equal(commonannotations.NodeNameAnnotation)))

				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// get updated NHC
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].Resource.GroupVersionKind()).To(Equal(newCr.GroupVersionKind()))
					g.Expect(underTest.Status.UnhealthyNodes[0].Remediations[1].TimedOut).ToNot(BeNil())
					g.Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseRemediating))

				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// Wait for 3rd remediation to start
				var thirdCR *unstructured.Unstructured
				Eventually(func(g Gomega) {
					thirdCR = getRemediationCRForMultiKindSupportTemplate(multiSupportTemplateRef.Name)
					g.Expect(thirdCR).ToNot(BeNil())
					g.Expect(thirdCR.GetName()).ToNot(Equal(unhealthyNodeName))
					g.Expect(thirdCR.GetAnnotations()[commonannotations.NodeNameAnnotation]).To(Equal(unhealthyNodeName))
					g.Expect(thirdCR.GetAnnotations()[annotations.TemplateNameAnnotation]).To(Equal(multiSupportTemplateRef.Name))
					g.Expect(thirdCR.GetAnnotations()).ToNot(HaveKey(Equal("remediation.medik8s.io/nhc-timed-out")))
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// Wait for 3nd remediation to time out
				Eventually(func(g Gomega) {
					// get updated CR
					thirdCR = getRemediationCRForMultiKindSupportTemplate(multiSupportTemplateRef.Name)
					g.Expect(thirdCR).ToNot(BeNil())
					g.Expect(thirdCR.GetName()).ToNot(Equal(unhealthyNodeName))
					g.Expect(thirdCR.GetAnnotations()[commonannotations.NodeNameAnnotation]).To(Equal(unhealthyNodeName))
					g.Expect(thirdCR.GetAnnotations()[annotations.TemplateNameAnnotation]).To(Equal(multiSupportTemplateRef.Name))
					g.Expect(thirdCR.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// Wait for 4th remediation to start
				var forthCR *unstructured.Unstructured
				Eventually(func(g Gomega) {
					forthCR = getRemediationCRForMultiKindSupportTemplate(secondMultiSupportTemplateRef.Name)
					g.Expect(forthCR).ToNot(BeNil())
					g.Expect(forthCR.GetName()).ToNot(Equal(unhealthyNodeName))
					g.Expect(forthCR.GetAnnotations()[commonannotations.NodeNameAnnotation]).To(Equal(unhealthyNodeName))
					g.Expect(forthCR.GetAnnotations()[annotations.TemplateNameAnnotation]).To(Equal(secondMultiSupportTemplateRef.Name))
					g.Expect(forthCR.GetAnnotations()).ToNot(HaveKey(Equal("remediation.medik8s.io/nhc-timed-out")))
				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				//Verify lease still exist (since long expire time wasn't reached)
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)).ToNot(HaveOccurred())
					g.Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(secondRemediationTimeout.Seconds() + mockLeaseBuffer.Seconds())))
					g.Expect(lease.Spec.AcquireTime).ToNot(BeNil())

				}, time.Second*10, time.Millisecond*300).Should(Succeed())

				// make node healthy
				node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: unhealthyNodeName}}
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Status.Conditions[0].Status = v1.ConditionTrue
				Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

				//calculating time left for lease
				timeLeftOnLease := time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second - time.Now().Sub(lease.Spec.RenewTime.Time)
				// wait a bit
				time.Sleep(2 * time.Second)
				timeLeftOnLease = timeLeftOnLease - time.Second*2
				//Verify lease has time left before it should expire
				Expect(timeLeftOnLease > time.Millisecond*500).To(BeTrue()) // a bit over 1 second at this stage
				//Verify lease was removed because the CR was deleted (even though there was some time left)
				err = k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseName, Namespace: leaseNs}, lease)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(*underTest.Status.HealthyNodes).To(Equal(3))
				Expect(*underTest.Status.ObservedNodes).To(Equal(3))
				Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(0))
				Expect(underTest.Status.Phase).To(Equal(v1alpha1.PhaseEnabled))

				// Ensure CRs are deleted
				Eventually(func(g Gomega) {
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					g.Expect(errors.IsNotFound(err)).To(BeTrue())
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(newCr), newCr)
					g.Expect(errors.IsNotFound(err)).To(BeTrue())
					g.Expect(getRemediationCRForMultiKindSupportTemplate(multiSupportTemplateRef.Name)).To(BeNil())
					g.Expect(getRemediationCRForMultiKindSupportTemplate(secondMultiSupportTemplateRef.Name)).To(BeNil())
				}, "5s", "200ms").Should(Succeed(), "CR wasn't deleted")
			})

			When("unhealthy condition changes", func() {

				It("should not proceed with next remediation when duration didn't expire yet", func() {
					// wait until nodes are unhealthy
					time.Sleep(nodeUnhealthyIn)

					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

					By("changing to other unhealthy condition")
					node := &v1.Node{}
					Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)).To(Succeed())
					Expect(node.Status.Conditions[0].Status).ToNot(Equal(v1.ConditionFalse))
					node.Status.Conditions[0].Status = v1.ConditionFalse
					node.Status.Conditions[0].LastTransitionTime = metav1.Now()
					Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

					By("ensuring CR isn't deleted")
					Consistently(func(g Gomega) {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					}, firstRemediationTimeout+2*time.Second, "500ms").Should(Succeed(), "CR was deleted")

					By("waiting for duration expiration")
					time.Sleep(unhealthyConditionDuration - firstRemediationTimeout)

					By("ensuring next remediation is processed now")
					// old CR timed out
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
					Expect(cr.GetAnnotations()).To(HaveKeyWithValue(Equal("remediation.medik8s.io/nhc-timed-out"), Not(BeNil())))
					// new CR created
					newCr := newRemediationCRForNHCSecondRemediation(unhealthyNodeName, underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(newCr), newCr)).To(Succeed())

					By("changing to healthy condition")
					Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: unhealthyNodeName}, node)).To(Succeed())
					node.Status.Conditions[0].Status = v1.ConditionTrue
					node.Status.Conditions[0].LastTransitionTime = metav1.Now()
					Expect(k8sClient.Status().Update(context.Background(), node)).To(Succeed())

					By("ensuring CRs are deleted")
					Eventually(func(g Gomega) {
						err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
						g.Expect(errors.IsNotFound(err)).To(BeTrue())
						err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(newCr), newCr)
						g.Expect(errors.IsNotFound(err)).To(BeTrue())
					}, "2s", "200ms").Should(Succeed(), "CR wasn't deleted")
				})

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
				cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
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

		Context("with expected permanent node deletion", func() {

			BeforeEach(func() {
				// TODO will work with classic remediation as well when https://github.com/medik8s/node-healthcheck-operator/pull/230 is merged
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

			deleteNode := func() {
				By("deleting the node")
				node := &v1.Node{}
				node.Name = unhealthyNodeName
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(node), node)).To(Succeed())
				Expect(k8sClient.Delete(context.Background(), node))
			}

			markCR := func() *unstructured.Unstructured {
				By("marking CR as succeeded and permanent node deletion expected")
				cr := newRemediationCRForNHC("unhealthy-worker-node-1", underTest)
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())
				conditions := []interface{}{
					map[string]interface{}{
						"type":               commonconditions.SucceededType,
						"status":             "True",
						"lastTransitionTime": time.Now().Format(time.RFC3339),
					},
					map[string]interface{}{
						"type":               commonconditions.PermanentNodeDeletionExpectedType,
						"status":             "True",
						"lastTransitionTime": time.Now().Format(time.RFC3339),
					},
				}
				unstructured.SetNestedSlice(cr.Object, conditions, "status", "conditions")
				Expect(k8sClient.Status().Update(context.Background(), cr))
				return cr
			}

			expectCRDeletion := func(cr *unstructured.Unstructured) {
				By("waiting for CR to be deleted")
				Eventually(func() bool {
					err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
					return errors.IsNotFound(err)
				}, "2s", "200ms").Should(BeTrue())

				// get updated NHC
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
				Expect(underTest.Status.UnhealthyNodes).To(BeEmpty())
			}

			It("it should delete orphaned CR when CR was updated", func() {
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				deleteNode()
				time.Sleep(1 * time.Second)
				cr := markCR()
				expectCRDeletion(cr)
			})

			It("it should delete orphaned CR when node is deleted", func() {
				Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
				cr := markCR()
				time.Sleep(1 * time.Second)
				deleteNode()
				expectCRDeletion(cr)
			})
		})

		Context("control plane nodes", func() {

			var pdb *policyv1.PodDisruptionBudget
			pdbSelector := map[string]string{
				"app": "guard",
			}

			BeforeEach(func() {
				// create etcd namespace
				ns := &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "openshift-etcd",
					},
				}
				// namespaces can't be deleted!
				err := k8sClient.Create(context.Background(), ns)
				if err != nil {
					Expect(errors.IsAlreadyExists(err)).To(BeTrue())
				}

				// create PDB
				pdb = &policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-name",
						Namespace: "openshift-etcd",
					},
					Spec: policyv1.PodDisruptionBudgetSpec{
						Selector: &metav1.LabelSelector{MatchLabels: pdbSelector},
					},
				}
				Expect(k8sClient.Create(context.Background(), pdb)).To(Succeed())
				DeferCleanup(func() {
					Expect(k8sClient.Delete(context.Background(), pdb)).To(Succeed())
				})

				// update pdb status
				Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
				pdb.Status.DisruptionsAllowed = 1
				Expect(k8sClient.Status().Update(context.Background(), pdb)).To(Succeed())

			})

			When("two control plane nodes are unhealthy, they should be remediated one after another", func() {
				BeforeEach(func() {
					objects = newNodes(2, 1, true, true)
					objects = append(objects, newNodes(1, 5, false, true)...)
					underTest = newNodeHealthCheck()
					objects = append(objects, underTest)
				})

				It("creates a one remediation CR for control plane node and updates status", func() {
					cr := newRemediationCRForNHC("", underTest)
					crList := &unstructured.UnstructuredList{Object: cr.Object}
					Expect(k8sClient.List(context.Background(), crList)).To(Succeed())

					Expect(len(crList.Items)).To(BeNumerically("==", 2), "expected 2 remediations, one for control plane, one for worker")
					Expect(crList.Items).To(ContainElements(
						// the unhealthy worker
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", unhealthyNodeName))),
						// one of the unhealthy control plane nodes
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", ContainSubstring("unhealthy-control-plane-node")))),
					))
					Expect(*underTest.Status.HealthyNodes).To(Equal(6))
					Expect(*underTest.Status.ObservedNodes).To(Equal(9))
					Expect(underTest.Status.InFlightRemediations).To(HaveLen(2))
					Expect(underTest.Status.UnhealthyNodes).To(HaveLen(3))
					Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
						And(
							HaveField("Name", unhealthyNodeName),
							HaveField("Remediations", ContainElement(
								And(
									HaveField("Resource.Name", unhealthyNodeName),
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

					var remediatedUnhealthyCPNodeName string
					for _, unhealthyNode := range underTest.Status.UnhealthyNodes {
						if strings.Contains(unhealthyNode.Name, "unhealthy-control-plane-node") {
							remediatedUnhealthyCPNodeName = unhealthyNode.Name
							break
						}
					}
					Expect(remediatedUnhealthyCPNodeName).ToNot(BeEmpty())

					By("simulating remediator by putting a finalizer on the cp remediation CR")
					unhealthyCPNodeCR := newRemediationCRForNHC(remediatedUnhealthyCPNodeName, underTest)
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyCPNodeCR), unhealthyCPNodeCR)).To(Succeed())
					unhealthyCPNodeCR.SetFinalizers([]string{"dummy"})
					Expect(k8sClient.Update(context.Background(), unhealthyCPNodeCR)).To(Succeed())

					By("make cp node healthy")
					unhealthyCPNode := &v1.Node{}
					unhealthyCPNode.Name = remediatedUnhealthyCPNodeName
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyCPNode), unhealthyCPNode)).To(Succeed())
					unhealthyCPNode.Status.Conditions = []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					}
					Expect(k8sClient.Status().Update(context.Background(), unhealthyCPNode))

					By("waiting for remediation end of cp node")
					Eventually(func(g Gomega) *metav1.Time {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyCPNodeCR), unhealthyCPNodeCR)).To(Succeed())
						return unhealthyCPNodeCR.GetDeletionTimestamp()
					}, "2s", "100ms").ShouldNot(BeNil(), "expected CR to be deleted")

					By("ensuring other cp node isn't remediated yet")
					Consistently(func(g Gomega) []*v1alpha1.UnhealthyNode {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
						return underTest.Status.UnhealthyNodes

					}, "5s", "1s").Should(HaveLen(3))

					By("simulating remediator finished by removing finalizer on the cp remediation CR")
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(unhealthyCPNodeCR), unhealthyCPNodeCR)).To(Succeed())
					unhealthyCPNodeCR.SetFinalizers([]string{})
					Expect(k8sClient.Update(context.Background(), unhealthyCPNodeCR)).To(Succeed())

					By("ensuring other cp node is remediated now")
					Eventually(func(g Gomega) []*v1alpha1.UnhealthyNode {
						g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(underTest), underTest)).To(Succeed())
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
							Not(HaveField("Name", remediatedUnhealthyCPNodeName)),
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
						HaveField("Object", HaveKeyWithValue("metadata", HaveKeyWithValue("name", remediatedUnhealthyCPNodeName))),
					))

				})
			})

			Context("one control plane node is unhealthy, and DisruptionsAllowed = 0", func() {
				BeforeEach(func() {
					objects = newNodes(1, 2, true, true)
					underTest = newNodeHealthCheck()
					objects = append(objects, underTest)

					// update pdb status
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
					pdb.Status.DisruptionsAllowed = 0
					Expect(k8sClient.Status().Update(context.Background(), pdb)).To(Succeed())
				})

				createGuardPod := func(isReady bool) {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "some-name",
							Namespace: pdb.Namespace,
							Labels:    pdbSelector,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test",
									Image: "test",
								},
							},
							NodeName: "unhealthy-control-plane-node-1",
						},
					}
					Expect(k8sClient.Create(context.Background(), pod)).To(Succeed())
					DeferCleanup(func() {
						Expect(k8sClient.Delete(context.Background(), pod, &client.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})).To(Succeed())
					})

					// update pod status
					Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
					status := v1.ConditionTrue
					if !isReady {
						status = v1.ConditionFalse
					}
					pod.Status.Conditions = []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: status,
						},
					}
					Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
				}

				When("unhealthy node is not disrupted already (guard pod is ready)", func() {

					BeforeEach(func() {
						// create ready guard pod
						createGuardPod(true)
					})

					It("doesn't create a remediation CR for control plane node", func() {
						cr := newRemediationCRForNHC("unhealthy-control-plane-node-1", underTest)
						crList := &unstructured.UnstructuredList{Object: cr.Object}
						Consistently(func(g Gomega) {
							g.Expect(k8sClient.List(context.Background(), crList)).To(Succeed())
							g.Expect(len(crList.Items)).To(BeNumerically("==", 0), "expected no remediation for cp node")
						}, "10s", "1s").Should(Succeed())
						Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						Expect(*underTest.Status.ObservedNodes).To(Equal(3))
						Expect(underTest.Status.InFlightRemediations).To(HaveLen(0))
						Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
							And(
								HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
								HaveField("Remediations", BeNil()),
							),
						))
					})
				})

				When("unhealthy node is disrupted already (guard pod is not ready)", func() {

					BeforeEach(func() {
						// create unready pod
						createGuardPod(false)
					})

					It("does create a remediation CR for control plane node", func() {
						cr := newRemediationCRForNHC("unhealthy-control-plane-node-1", underTest)
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

						Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						Expect(*underTest.Status.ObservedNodes).To(Equal(3))
						Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
							And(
								HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
								HaveField("Remediations", Not(BeNil())),
							),
						))
					})
				})

				When("unhealthy node has no guard pod (node doesn't run etcd or guard pod was deleted)", func() {

					It("does create a remediation CR for control plane node", func() {
						cr := newRemediationCRForNHC("unhealthy-control-plane-node-1", underTest)
						Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)).To(Succeed())

						Expect(*underTest.Status.HealthyNodes).To(Equal(2))
						Expect(*underTest.Status.ObservedNodes).To(Equal(3))
						Expect(underTest.Status.InFlightRemediations).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes).To(HaveLen(1))
						Expect(underTest.Status.UnhealthyNodes).To(ContainElements(
							And(
								HaveField("Name", ContainSubstring("unhealthy-control-plane-node")),
								HaveField("Remediations", Not(BeNil())),
							),
						))
					})
				})
			})

		})

		When("remediation is needed but pauseRequests exists", func() {
			BeforeEach(func() {
				setupObjects(1, 2, true)
				underTest.Spec.PauseRequests = []string{"I'm an admin, asking you to stop remediating this group of nodes"}
			})

			It("skips remediation and updates status", func() {
				cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(0))
				Expect(*underTest.Status.ObservedNodes).To(Equal(0))
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
				cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
				Expect(errors.IsNotFound(err)).To(BeTrue())

				Expect(*underTest.Status.HealthyNodes).To(Equal(0))
				Expect(*underTest.Status.ObservedNodes).To(Equal(0))
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
						if o.GetName() == unhealthyNodeName {
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
					cr := newRemediationCRForNHC(unhealthyNodeName, underTest)
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
				requests := handler(context.TODO(), &updatedNode)
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
				requests := handler(context.TODO(), &updatedNode)
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
				requests := handler(context.TODO(), &updatedNode)
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

	Context("Unhealthy condition checks", func() {

		var (
			r = &NodeHealthCheckReconciler{
				Recorder: record.NewFakeRecorder(1),
			}
			nhc            = newNodeHealthCheck()
			nodeConditions []v1.NodeCondition
			node           *v1.Node

			condType1         = v1.NodeConditionType("type1")
			condType2         = v1.NodeConditionType("type2")
			condStatusMatch   = v1.ConditionTrue
			condStatusNoMatch = v1.ConditionUnknown

			now                      = time.Now()
			unhealthyDuration        = metav1.Duration{Duration: 10 * time.Second}
			expireIn                 = 2 * time.Second
			expiredTransitionTime    = metav1.Time{Time: now.Add(-unhealthyDuration.Duration).Add(-time.Second)}
			notExpiredTransitionTime = metav1.Time{Time: now.Add(-unhealthyDuration.Duration).Add(expireIn)}

			// this is always added in tested code
			expireBuffer = time.Second
		)

		BeforeEach(func() {
			fakeTime = &now
			DeferCleanup(func() {
				fakeTime = nil
			})

			nhc.Spec.UnhealthyConditions = []v1alpha1.UnhealthyCondition{
				{
					Type:     condType1,
					Status:   condStatusMatch,
					Duration: unhealthyDuration,
				},
				{
					Type:     condType2,
					Status:   condStatusMatch,
					Duration: unhealthyDuration,
				},
			}
		})

		JustBeforeEach(func() {
			node = &v1.Node{}
			node.Name = "test-node"
			node.Status.Conditions = nodeConditions
		})

		When("no condition matches", func() {
			BeforeEach(func() {
				nodeConditions = []v1.NodeCondition{
					{
						Type:               condType1,
						Status:             condStatusNoMatch,
						LastTransitionTime: notExpiredTransitionTime,
					},
					{
						Type:               condType2,
						Status:             condStatusNoMatch,
						LastTransitionTime: expiredTransitionTime,
					},
				}
			})
			It("should not report match, should not report expiry", func() {
				match, expire := r.matchesUnhealthyConditions(nhc, node)
				Expect(match).To(BeFalse(), "expected healthy")
				Expect(expire).To(BeNil(), "expected expire to not be set")
			})
		})

		When("a single condition matches but didn't expire", func() {
			BeforeEach(func() {
				nodeConditions = []v1.NodeCondition{
					{
						Type:               condType1,
						Status:             condStatusMatch,
						LastTransitionTime: notExpiredTransitionTime,
					},
				}
			})
			It("should not report match, should report expiry", func() {
				match, expire := r.matchesUnhealthyConditions(nhc, node)
				Expect(match).To(BeFalse(), "expected healthy")
				Expect(expire).ToNot(BeNil(), "expected expire to be set")
				Expect(*expire).To(Equal(expireIn+expireBuffer), "expected expire in 1 second")
			})
		})

		When("first condition matches but didn't expire, second condition matches and expired", func() {
			BeforeEach(func() {
				nodeConditions = []v1.NodeCondition{
					{
						Type:               condType1,
						Status:             condStatusMatch,
						LastTransitionTime: notExpiredTransitionTime,
					},
					{
						Type:               condType2,
						Status:             condStatusMatch,
						LastTransitionTime: expiredTransitionTime,
					},
				}
			})
			It("should report match, should not report expiry", func() {
				match, expire := r.matchesUnhealthyConditions(nhc, node)
				Expect(match).To(BeTrue(), "expected not healthy")
				Expect(expire).To(BeNil(), "expected expire to not be set")
			})
		})

		When("first condition doesn't match, second condition matches and didn't expire", func() {
			BeforeEach(func() {
				nodeConditions = []v1.NodeCondition{
					{
						Type:               condType1,
						Status:             condStatusNoMatch,
						LastTransitionTime: notExpiredTransitionTime,
					},
					{
						Type:               condType2,
						Status:             condStatusMatch,
						LastTransitionTime: notExpiredTransitionTime,
					},
				}
			})
			It("should not report match, should not report expiry", func() {
				match, expire := r.matchesUnhealthyConditions(nhc, node)
				Expect(match).To(BeFalse(), "expected healthy")
				Expect(expire).ToNot(BeNil(), "expected expire to be set")
				Expect(*expire).To(Equal(expireIn+expireBuffer), "expected expire in 1 second")
			})
		})

	})
})

func mockLeaseParams(mockRequeueDurationIfLeaseTaken, mockDefaultLeaseDuration, mockLeaseBuffer time.Duration) {
	orgRequeueIfLeaseTaken := resources.RequeueIfLeaseTaken
	orgDefaultLeaseDuration := utils.DefaultRemediationDuration
	orgLeaseBuffer := resources.LeaseBuffer
	//set up mock values so tests can run in a reasonable time
	resources.RequeueIfLeaseTaken = mockRequeueDurationIfLeaseTaken
	utils.DefaultRemediationDuration = mockDefaultLeaseDuration
	resources.LeaseBuffer = mockLeaseBuffer

	ns := &v1.Namespace{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: leaseNs}, ns); err != nil {
		if errors.IsNotFound(err) {
			ns.Name = leaseNs
			err := k8sClient.Create(context.Background(), ns)
			Expect(err).ToNot(HaveOccurred())
		}

	}

	DeferCleanup(func() {
		resources.RequeueIfLeaseTaken = orgRequeueIfLeaseTaken
		utils.DefaultRemediationDuration = orgDefaultLeaseDuration
		resources.LeaseBuffer = orgLeaseBuffer
	})
}

func newRemediationCRForNHC(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	var templateRef v1.ObjectReference
	if nhc.Spec.RemediationTemplate != nil {
		templateRef = *nhc.Spec.RemediationTemplate
	} else {
		templateRef = nhc.Spec.EscalatingRemediations[0].RemediationTemplate
	}
	owner := metav1.OwnerReference{
		APIVersion: nhc.APIVersion,
		Kind:       nhc.Kind,
		Name:       nhc.Name,
		UID:        nhc.UID,
	}
	return newRemediationCR(nodeName, templateRef, owner)
}

func newRemediationCRForNHCSecondRemediation(nodeName string, nhc *v1alpha1.NodeHealthCheck) *unstructured.Unstructured {
	templateRef := nhc.Spec.EscalatingRemediations[1].RemediationTemplate
	owner := metav1.OwnerReference{
		APIVersion: nhc.APIVersion,
		Kind:       nhc.Kind,
		Name:       nhc.Name,
		UID:        nhc.UID,
	}
	return newRemediationCR(nodeName, templateRef, owner)
}

func getRemediationCRForMultiKindSupportTemplate(templateName string) *unstructured.Unstructured {
	gvk := schema.GroupVersionKind{
		Group:   multiSupportTemplateRef.GroupVersionKind().Group,
		Version: multiSupportTemplateRef.GroupVersionKind().Version,
		Kind:    strings.TrimSuffix(multiSupportTemplateRef.GroupVersionKind().Kind, "Template"),
	}
	remediationCRBase := &unstructured.Unstructured{}
	remediationCRBase.SetGroupVersionKind(gvk)
	resourceList := &unstructured.UnstructuredList{Object: remediationCRBase.Object}
	if err := k8sClient.List(ctx, resourceList); err == nil {
		for _, cr := range resourceList.Items {
			ann := cr.GetAnnotations()
			if ann != nil {
				if ann[annotations.TemplateNameAnnotation] == templateName {
					return &cr
				}
			}
		}
	}
	return nil
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
				{
					Type:     v1.NodeReady,
					Status:   v1.ConditionUnknown,
					Duration: metav1.Duration{Duration: unhealthyConditionDuration},
				},
			},
			RemediationTemplate: infraRemediationTemplateRef.DeepCopy(),
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
		node := newNode(fmt.Sprintf("unhealthy%s-node-%d", roleName, i), v1.NodeReady, v1.ConditionUnknown, isControlPlane, unhealthyNow)
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
		labels[commonLabels.ControlPlaneRole] = ""
	} else {
		labels[commonLabels.WorkerRole] = ""
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
