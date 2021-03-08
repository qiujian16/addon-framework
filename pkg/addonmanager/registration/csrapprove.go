package registration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	authorizationv1 "k8s.io/api/authorization/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/open-cluster-management/addon-framework/pkg/addoninterface"
	"github.com/open-cluster-management/addon-framework/pkg/helpers"
)

const (
	spokeClusterNameLabel = "open-cluster-management.io/cluster-name"
)

// csrApprovingController auto approve the renewal CertificateSigningRequests for an accepted spoke cluster on the hub.
type csrApprovingController struct {
	kubeClient        kubernetes.Interface
	approveCheckFuncs []addoninterface.CSRApproveCheckFunc
	renewCheckFuncs   []addoninterface.CSRApproveCheckFunc
	signer            string
	addonName         string
	eventRecorder     events.Recorder
}

// NewCSRApprovingController creates a new csr approving controller
func NewCSRApprovingController(
	kubeClient kubernetes.Interface,
	csrInformer factory.Informer,
	recorder events.Recorder,
	signer string,
	addonName string,
	checkFuncs ...addoninterface.CSRApproveCheckFunc,
) factory.Controller {
	c := &csrApprovingController{
		kubeClient: kubeClient,
		signer:     signer,
		addonName:  addonName,
		renewCheckFuncs: []addoninterface.CSRApproveCheckFunc{
			IsCSRInTerminalState,
		},
		approveCheckFuncs: checkFuncs,
		eventRecorder:     recorder.WithComponentSuffix(fmt.Sprintf("%s-csr-approving-controller", addonName)),
	}
	c.renewCheckFuncs = append(c.renewCheckFuncs, c.isSpokeClusterClientCertRenewal)
	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			},
			func(obj interface{}) bool {
				accessor, _ := meta.Accessor(obj)
				if strings.HasPrefix(accessor.GetName(), helpers.AgentCSRGenerateName("", addonName)) {
					return true
				}
				return false
			},
			csrInformer).
		WithSync(c.sync).
		ToController("CSRApprovingController", recorder)
}

func (c *csrApprovingController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	csrName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling CertificateSigningRequests %q", csrName)
	csr, err := c.kubeClient.CertificatesV1().CertificateSigningRequests().Get(ctx, csrName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	csr = csr.DeepCopy()

	if isCSRApproved(csr) {
		return nil
	}
	// Check if csr can be approved
	var approved bool
	for _, checkFunc := range c.approveCheckFuncs {
		approved = checkFunc(csr)
		if !approved {
			break
		}
	}

	// Check if csr renew can be approved
	var renewApproved bool
	for _, renewCheck := range c.renewCheckFuncs {
		renewApproved = renewCheck(csr)
		if !renewApproved {
			break
		}
	}

	// Do not approve if csr check fails and it is not csr renewal
	if !approved && !renewApproved {
		klog.V(4).Infof("addon csr %q cannont be auto approved due to approve check fails", csr.Name)
		return nil
	}

	if renewApproved {
		// Authorize whether the current spoke agent has been authorized to renew its csr.
		allowed, err := c.authorize(ctx, c.addonName, csr)
		if err != nil {
			return err
		}
		if !allowed {
			//TODO find a way to avoid looking at this CSR again.
			klog.V(4).Infof("addon csr %q cannont be auto approved due to subject access review was not approved", csr.Name)
			return nil
		}
	}

	// Auto approve the spoke cluster csr
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:    certificatesv1.CertificateApproved,
		Status:  corev1.ConditionTrue,
		Reason:  "AutoApprovedByHubCSRApprovingController",
		Message: "Auto approving addon agent certificate after SubjectAccessReview.",
	})
	_, err = c.kubeClient.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	c.eventRecorder.Eventf("AddonCSRAutoApproved", "addon csr %q is auto approved by addon csr controller", csr.Name)
	return nil
}

// Using SubjectAccessReview API to check whether a spoke agent has been authorized to renew its csr,
// a spoke agent is authorized after its spoke cluster is accepted by hub cluster admin.
func (c *csrApprovingController) authorize(ctx context.Context, addonName string, csr *certificatesv1.CertificateSigningRequest) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue)
	for k, v := range csr.Spec.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   csr.Spec.Username,
			UID:    csr.Spec.UID,
			Groups: csr.Spec.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:       "register.open-cluster-management.io",
				Resource:    addonName,
				Verb:        "renew",
				Subresource: "clientcertificates",
			},
		},
	}
	sar, err := c.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

// To check a renewal managed cluster csr, we check
// 1. if the signer name in csr request is valid.
// 2. if organization field and commonName field in csr request is valid.
// 3. if user name in csr is the same as commonName field in csr request.
func (c *csrApprovingController) isSpokeClusterClientCertRenewal(csr *certificatesv1.CertificateSigningRequest) bool {
	spokeClusterName, existed := csr.Labels[spokeClusterNameLabel]
	if !existed {
		return false
	}

	// The CSR signer name must be provided on Kubernetes v1.18.0 and above, so if the signer name is empty,
	// we should be on an old server, we skip the signer name check
	if len(csr.Spec.SignerName) != 0 &&
		csr.Spec.SignerName != c.signer {
		return false
	}

	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		klog.V(4).Infof("csr %q was not recognized: PEM block type is not CERTIFICATE REQUEST", csr.Name)
		return false
	}

	x509cr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		klog.V(4).Infof("csr %q was not recognized: %v", csr.Name, err)
		return false
	}

	if len(x509cr.Subject.Organization) != 1 {
		return false
	}

	organization := x509cr.Subject.Organization[0]
	if organization != helpers.AgentGroup(spokeClusterName, c.addonName) {
		return false
	}

	if !strings.HasPrefix(x509cr.Subject.CommonName, organization) {
		return false
	}

	return csr.Spec.Username == x509cr.Subject.CommonName
}

// Check whether a CSR is in terminal state
func IsCSRInTerminalState(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved {
			return true
		}
		if c.Type == certificatesv1.CertificateDenied {
			return true
		}
	}
	return false
}

func isCSRApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	// TODO: need to make it work in csr v1 as well
	approved := false
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateDenied {
			return false
		} else if condition.Type == certificatesv1.CertificateApproved {
			approved = true
		}
	}

	return approved
}
