/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reallocation

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha3"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"golang.org/x/time/rate"
	"knative.dev/pkg/logging"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Controller for the resource
type Controller struct {
	Utilization   *Utilization
	CloudProvider cloudprovider.CloudProvider
	KubeClient    client.Client
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		Utilization:   &Utilization{KubeClient: kubeClient},
		CloudProvider: cloudProvider,
		KubeClient:    kubeClient,
	}
}

// Reconcile executes a reallocation control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("Reallocation"))

	// 1. Retrieve provisioner from reconcile request
	provisioner := &v1alpha3.Provisioner{}
	if err := c.KubeClient.Get(ctx, req.NamespacedName, provisioner); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// 2. Delete any node that has been unable to join.
	if err := c.Utilization.terminateFailedToJoin(ctx, provisioner); err != nil {
		return reconcile.Result{}, fmt.Errorf("terminating nodes that failed to join, %w", err)
	}

	// Skip reconciliation if utilization ttl is not defined.
	if provisioner.Spec.TTLSecondsAfterEmpty == nil {
		return reconcile.Result{}, nil
	}

	// 3. Set TTL on TTLable Nodes
	if err := c.Utilization.markUnderutilized(ctx, provisioner); err != nil {
		return reconcile.Result{}, fmt.Errorf("adding ttl and underutilized label, %w", err)
	}

	// 4. Remove TTL from Utilized Nodes
	if err := c.Utilization.clearUnderutilized(ctx, provisioner); err != nil {
		return reconcile.Result{}, fmt.Errorf("removing ttl from node, %w", err)
	}

	// 5. Delete any node past its TTL
	if err := c.Utilization.terminateExpired(ctx, provisioner); err != nil {
		return reconcile.Result{}, fmt.Errorf("marking nodes terminable, %w", err)
	}

	return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named("Reallocation").
		For(&v1alpha3.Provisioner{}).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewMaxOfRateLimiter(
					workqueue.NewItemExponentialFailureRateLimiter(100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 1,
			},
		).
		Complete(c)
}
