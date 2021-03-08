package lease

import (
	"context"
	"fmt"
	"sync"
	"time"

	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"

	"github.com/openshift/library-go/pkg/operator/events"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
)

const leaseUpdateJitterFactor = 0.25

// managedClusterLeaseController periodically updates the lease of a managed cluster on hub cluster to keep the heartbeat of a managed cluster.
type managedClusterLeaseController struct {
	clusterName              string
	hubClient                clientset.Interface
	addonLister              addonlisterv1alpha1.ManagedClusterAddOnLister
	lastLeaseDurationSeconds int32
	leaseUpdater             *leaseUpdater
}

// leaseUpdater periodically updates the lease of a managed cluster
type leaseUpdater struct {
	hubClient   clientset.Interface
	clusterName string
	leaseName   string
	lock        sync.Mutex
	cancel      context.CancelFunc
	recorder    events.Recorder
}

func NewLeaseUpdater(clusterName, leaseName string, hubClient clientset.Interface, recorder events.Recorder) *leaseUpdater {
	return &leaseUpdater{
		clusterName: clusterName,
		leaseName:   leaseName,
		recorder:    recorder,
		hubClient:   hubClient,
	}
}

// start a lease update routine to update the lease of a managed cluster periodically.
func (u *leaseUpdater) Start(ctx context.Context, leaseDuration time.Duration) {
	u.lock.Lock()
	defer u.lock.Unlock()

	var updateCtx context.Context
	updateCtx, u.cancel = context.WithCancel(ctx)
	go wait.JitterUntilWithContext(updateCtx, u.update, leaseDuration, leaseUpdateJitterFactor, true)
	u.recorder.Eventf("ManagedClusterLeaseUpdateStrated", "Start to update lease %q on cluster %q", u.leaseName, u.clusterName)
}

// stop the lease update routine.
func (u *leaseUpdater) stop() {
	u.lock.Lock()
	defer u.lock.Unlock()

	if u.cancel == nil {
		return
	}
	u.cancel()
	u.cancel = nil
	u.recorder.Eventf("ManagedClusterLeaseUpdateStoped", "Stop to update lease %q on cluster %q", u.leaseName, u.clusterName)
}

// update the lease of a given managed cluster.
func (u *leaseUpdater) update(ctx context.Context) {
	lease, err := u.hubClient.CoordinationV1().Leases(u.clusterName).Get(ctx, u.leaseName, metav1.GetOptions{})
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to get cluster lease %q on hub cluster: %w", u.leaseName, err))
		return
	}

	lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
	if _, err = u.hubClient.CoordinationV1().Leases(u.clusterName).Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to update cluster lease %q on hub cluster: %w", u.leaseName, err))
	}
}
