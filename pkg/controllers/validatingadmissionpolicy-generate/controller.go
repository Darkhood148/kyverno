package validatingadmissionpolicygenerate

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov2 "github.com/kyverno/kyverno/api/kyverno/v2"
	"github.com/kyverno/kyverno/pkg/admissionpolicy"
	"github.com/kyverno/kyverno/pkg/auth/checker"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	kyvernov1informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v1"
	kyvernov2informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v2"
	kyvernov1listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1"
	kyvernov2listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v2"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/controllers"
	"github.com/kyverno/kyverno/pkg/event"
	"github.com/kyverno/kyverno/pkg/logging"
	controllerutils "github.com/kyverno/kyverno/pkg/utils/controller"
	datautils "github.com/kyverno/kyverno/pkg/utils/data"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	admissionregistrationv1informers "k8s.io/client-go/informers/admissionregistration/v1"
	"k8s.io/client-go/kubernetes"
	admissionregistrationv1listers "k8s.io/client-go/listers/admissionregistration/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// Workers is the number of workers for this controller
	Workers        = 2
	ControllerName = "validatingadmissionpolicy-generate-controller"
	maxRetries     = 10
)

type controller struct {
	// clients
	client          kubernetes.Interface
	kyvernoClient   versioned.Interface
	discoveryClient dclient.IDiscovery

	// listers
	cpolLister       kyvernov1listers.ClusterPolicyLister
	polexLister      kyvernov2listers.PolicyExceptionLister
	vapLister        admissionregistrationv1listers.ValidatingAdmissionPolicyLister
	vapbindingLister admissionregistrationv1listers.ValidatingAdmissionPolicyBindingLister

	// queue
	queue workqueue.TypedRateLimitingInterface[any]

	eventGen event.Interface
	checker  checker.AuthChecker
}

func NewController(
	client kubernetes.Interface,
	kyvernoClient versioned.Interface,
	discoveryClient dclient.IDiscovery,
	cpolInformer kyvernov1informers.ClusterPolicyInformer,
	polexInformer kyvernov2informers.PolicyExceptionInformer,
	vapInformer admissionregistrationv1informers.ValidatingAdmissionPolicyInformer,
	vapbindingInformer admissionregistrationv1informers.ValidatingAdmissionPolicyBindingInformer,
	eventGen event.Interface,
	checker checker.AuthChecker,
) controllers.Controller {
	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[any](),
		workqueue.TypedRateLimitingQueueConfig[any]{Name: ControllerName},
	)
	c := &controller{
		client:           client,
		kyvernoClient:    kyvernoClient,
		discoveryClient:  discoveryClient,
		cpolLister:       cpolInformer.Lister(),
		polexLister:      polexInformer.Lister(),
		vapLister:        vapInformer.Lister(),
		vapbindingLister: vapbindingInformer.Lister(),
		queue:            queue,
		eventGen:         eventGen,
		checker:          checker,
	}

	// Set up an event handler for when Kyverno policies change
	if _, err := controllerutils.AddEventHandlersT(cpolInformer.Informer(), c.addPolicy, c.updatePolicy, c.deletePolicy); err != nil {
		logger.Error(err, "failed to register event handlers")
	}

	// Set up an event handler for when policy exceptions change
	if _, err := controllerutils.AddEventHandlersT(polexInformer.Informer(), c.addException, c.updateException, c.deleteException); err != nil {
		logger.Error(err, "failed to register event handlers")
	}

	// Set up an event handler for when validating admission policies change
	if _, err := controllerutils.AddEventHandlersT(vapInformer.Informer(), c.addVAP, c.updateVAP, c.deleteVAP); err != nil {
		logger.Error(err, "failed to register event handlers")
	}

	// Set up an event handler for when validating admission policy bindings change
	if _, err := controllerutils.AddEventHandlersT(vapbindingInformer.Informer(), c.addVAPbinding, c.updateVAPbinding, c.deleteVAPbinding); err != nil {
		logger.Error(err, "failed to register event handlers")
	}

	return c
}

func (c *controller) Run(ctx context.Context, workers int) {
	controllerutils.Run(ctx, logger, ControllerName, time.Second, c.queue, workers, maxRetries, c.reconcile)
}

func (c *controller) addPolicy(obj kyvernov1.PolicyInterface) {
	logger.V(2).Info("policy created", "uid", obj.GetUID(), "kind", obj.GetKind(), "name", obj.GetName())
	c.enqueuePolicy(obj)
}

func (c *controller) updatePolicy(old, obj kyvernov1.PolicyInterface) {
	if datautils.DeepEqual(old.GetSpec(), obj.GetSpec()) {
		return
	}
	logger.V(2).Info("policy updated", "uid", obj.GetUID(), "kind", obj.GetKind(), "name", obj.GetName())
	c.enqueuePolicy(obj)
}

func (c *controller) deletePolicy(obj kyvernov1.PolicyInterface) {
	var p kyvernov1.PolicyInterface

	switch kubeutils.GetObjectWithTombstone(obj).(type) {
	case *kyvernov1.ClusterPolicy:
		p = kubeutils.GetObjectWithTombstone(obj).(*kyvernov1.ClusterPolicy)
	default:
		logger.Info("Failed to get deleted object", "obj", obj)
		return
	}

	logger.V(2).Info("policy deleted", "uid", p.GetUID(), "kind", p.GetKind(), "name", p.GetName())
	c.enqueuePolicy(obj)
}

func (c *controller) enqueuePolicy(obj kyvernov1.PolicyInterface) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Error(err, "failed to extract policy name")
		return
	}
	c.queue.Add(key)
}

func (c *controller) addException(obj *kyvernov2.PolicyException) {
	logger.V(2).Info("policy exception created", "uid", obj.GetUID(), "kind", obj.GetKind(), "name", obj.GetName())
	c.enqueueException(obj)
}

func (c *controller) updateException(old, obj *kyvernov2.PolicyException) {
	if datautils.DeepEqual(old.Spec, obj.Spec) {
		return
	}
	logger.V(2).Info("policy exception updated", "uid", obj.GetUID(), "kind", obj.GetKind(), "name", obj.GetName())
	c.enqueueException(obj)
}

func (c *controller) deleteException(obj *kyvernov2.PolicyException) {
	polex := kubeutils.GetObjectWithTombstone(obj).(*kyvernov2.PolicyException)

	logger.V(2).Info("policy exception deleted", "uid", polex.GetUID(), "kind", polex.GetKind(), "name", polex.GetName())
	c.enqueueException(obj)
}

func (c *controller) enqueueException(obj *kyvernov2.PolicyException) {
	for _, exception := range obj.Spec.Exceptions {
		// skip adding namespaced policies in the queue.
		// skip adding policies with multiple rules in the queue.
		if strings.Contains(exception.PolicyName, "/") || len(exception.RuleNames) > 1 {
			continue
		}

		cpol, err := c.getClusterPolicy(exception.PolicyName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return
			}
			logger.Error(err, "unable to get the policy from policy informer")
			return
		}
		c.enqueuePolicy(cpol)
	}
}

func (c *controller) addVAP(obj *admissionregistrationv1.ValidatingAdmissionPolicy) {
	c.enqueueVAP(obj)
}

func (c *controller) updateVAP(old, obj *admissionregistrationv1.ValidatingAdmissionPolicy) {
	if datautils.DeepEqual(old.Spec, obj.Spec) {
		return
	}
	c.enqueueVAP(obj)
}

func (c *controller) deleteVAP(obj *admissionregistrationv1.ValidatingAdmissionPolicy) {
	c.enqueueVAP(obj)
}

func (c *controller) enqueueVAP(v *admissionregistrationv1.ValidatingAdmissionPolicy) {
	if len(v.OwnerReferences) == 1 {
		if v.OwnerReferences[0].Kind == "ClusterPolicy" {
			cpol, err := c.cpolLister.Get(v.OwnerReferences[0].Name)
			if err != nil {
				return
			}
			c.enqueuePolicy(cpol)
		}
	}
}

func (c *controller) addVAPbinding(obj *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	c.enqueueVAPbinding(obj)
}

func (c *controller) updateVAPbinding(old, obj *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	if datautils.DeepEqual(old.Spec, obj.Spec) {
		return
	}
	c.enqueueVAPbinding(obj)
}

func (c *controller) deleteVAPbinding(obj *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	c.enqueueVAPbinding(obj)
}

func (c *controller) enqueueVAPbinding(vb *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	if len(vb.OwnerReferences) == 1 {
		if vb.OwnerReferences[0].Kind == "ClusterPolicy" {
			cpol, err := c.cpolLister.Get(vb.OwnerReferences[0].Name)
			if err != nil {
				return
			}
			c.enqueuePolicy(cpol)
		}
	}
}

func (c *controller) getClusterPolicy(name string) (*kyvernov1.ClusterPolicy, error) {
	cpolicy, err := c.cpolLister.Get(name)
	if err != nil {
		return nil, err
	}
	return cpolicy, nil
}

func (c *controller) getValidatingAdmissionPolicy(name string) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	vap, err := c.vapLister.Get(name)
	if err != nil {
		return nil, err
	}
	return vap, nil
}

func (c *controller) getValidatingAdmissionPolicyBinding(name string) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	vapbinding, err := c.vapbindingLister.Get(name)
	if err != nil {
		return nil, err
	}
	return vapbinding, nil
}

// getExceptions get exceptions that match both the policy and the rule if exists.
func (c *controller) getExceptions(policyName, rule string) ([]kyvernov2.PolicyException, error) {
	var exceptions []kyvernov2.PolicyException
	polexs, err := c.polexLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, polex := range polexs {
		if polex.Contains(policyName, rule) {
			exceptions = append(exceptions, *polex)
		}
	}
	return exceptions, nil
}

func constructVapBindingName(vapName string) string {
	return vapName + "-binding"
}

func (c *controller) reconcile(ctx context.Context, logger logr.Logger, key, namespace, name string) error {
	policy, err := c.getClusterPolicy(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		logger.Error(err, "unable to get the policy from policy informer")
		return err
	}

	spec := policy.GetSpec()
	if !spec.HasValidate() {
		return nil
	}

	// check if the controller has the required permissions to generate validating admission policies.
	if !admissionpolicy.HasValidatingAdmissionPolicyPermission(c.checker) {
		logger.V(2).Info("insufficient permissions to generate ValidatingAdmissionPolicies")
		c.updateClusterPolicyStatus(ctx, *policy, false, "insufficient permissions to generate ValidatingAdmissionPolicies")
		return nil
	}

	// check if the controller has the required permissions to generate validating admission policy bindings.
	if !admissionpolicy.HasValidatingAdmissionPolicyBindingPermission(c.checker) {
		logger.V(2).Info("insufficient permissions to generate ValidatingAdmissionPolicyBindings")
		c.updateClusterPolicyStatus(ctx, *policy, false, "insufficient permissions to generate ValidatingAdmissionPolicyBindings")
		return nil
	}

	vapName := policy.GetName()
	vapBindingName := constructVapBindingName(vapName)

	observedVAP, vapErr := c.getValidatingAdmissionPolicy(vapName)
	observedVAPbinding, vapBindingErr := c.getValidatingAdmissionPolicyBinding(vapBindingName)

	exceptions, err := c.getExceptions(name, spec.Rules[0].Name)
	if err != nil {
		return err
	}

	if ok, msg := admissionpolicy.CanGenerateVAP(spec, exceptions); !ok {
		// delete the ValidatingAdmissionPolicy if exist
		if vapErr == nil {
			err = c.client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, vapName, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}
		// delete the ValidatingAdmissionPolicyBinding if exist
		if vapBindingErr == nil {
			err = c.client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(ctx, vapBindingName, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}

		if msg == "" {
			msg = "skip generating ValidatingAdmissionPolicy: a policy exception is configured."
		}
		c.updateClusterPolicyStatus(ctx, *policy, false, msg)
		return nil
	}

	if vapErr != nil {
		if !apierrors.IsNotFound(vapErr) {
			c.updateClusterPolicyStatus(ctx, *policy, false, vapErr.Error())
			return vapErr
		}
		observedVAP = &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: vapName,
			},
		}
	}

	if vapBindingErr != nil {
		if !apierrors.IsNotFound(vapBindingErr) {
			c.updateClusterPolicyStatus(ctx, *policy, false, vapBindingErr.Error())
			return vapBindingErr
		}
		observedVAPbinding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: vapBindingName,
			},
		}
	}

	if observedVAP.ResourceVersion == "" {
		err := admissionpolicy.BuildValidatingAdmissionPolicy(c.discoveryClient, observedVAP, policy, exceptions)
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
		_, err = c.client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, observedVAP, metav1.CreateOptions{})
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
	} else {
		_, err = controllerutils.Update(
			ctx,
			observedVAP,
			c.client.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
			func(observed *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return admissionpolicy.BuildValidatingAdmissionPolicy(c.discoveryClient, observed, policy, exceptions)
			})
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
	}

	if observedVAPbinding.ResourceVersion == "" {
		err := admissionpolicy.BuildValidatingAdmissionPolicyBinding(observedVAPbinding, policy)
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
		_, err = c.client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, observedVAPbinding, metav1.CreateOptions{})
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
	} else {
		_, err = controllerutils.Update(
			ctx,
			observedVAPbinding,
			c.client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings(),
			func(observed *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return admissionpolicy.BuildValidatingAdmissionPolicyBinding(observed, policy)
			})
		if err != nil {
			c.updateClusterPolicyStatus(ctx, *policy, false, err.Error())
			return err
		}
	}

	c.updateClusterPolicyStatus(ctx, *policy, true, "")
	// generate events
	e := event.NewValidatingAdmissionPolicyEvent(policy, observedVAP.Name, observedVAPbinding.Name)
	c.eventGen.Add(e...)
	return nil
}

func (c *controller) updateClusterPolicyStatus(ctx context.Context, cpol kyvernov1.ClusterPolicy, generated bool, msg string) {
	latest := cpol.DeepCopy()
	latest.Status.ValidatingAdmissionPolicy.Generated = generated
	latest.Status.ValidatingAdmissionPolicy.Message = msg

	new, _ := c.kyvernoClient.KyvernoV1().ClusterPolicies().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
	logging.V(3).Info("updated kyverno policy status", "name", cpol.GetName(), "status", new.Status)
}
