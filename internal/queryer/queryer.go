package queryer

import (
	"github.com/heptio/developer-dash/internal/cache"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/core"
)

//go:generate mockgen -destination=./fake/mock_queryer.go -package=fake github.com/heptio/developer-dash/internal/queryer Queryer
//go:generate mockgen -destination=./fake/mock_discovery.go -package=fake k8s.io/client-go/discovery DiscoveryInterface

type Queryer interface {
	Children(object metav1.Object) ([]runtime.Object, error)
	Events(object metav1.Object) ([]*corev1.Event, error)
	IngressesForService(service *corev1.Service) ([]*extv1beta1.Ingress, error)
	OwnerReference(namespace string, ownerReference metav1.OwnerReference) (runtime.Object, error)
	PodsForService(service *corev1.Service) ([]*corev1.Pod, error)
	ServicesForIngress(ingress *extv1beta1.Ingress) ([]*corev1.Service, error)
	ServicesForPod(pod *corev1.Pod) ([]*corev1.Service, error)
}

type CacheQueryer struct {
	cache           cache.Cache
	discoveryClient discovery.DiscoveryInterface
}

var _ Queryer = (*CacheQueryer)(nil)

func New(c cache.Cache, discoveryClient discovery.DiscoveryInterface) *CacheQueryer {
	return &CacheQueryer{
		cache:           c,
		discoveryClient: discoveryClient,
	}
}

func (cq *CacheQueryer) Children(owner metav1.Object) ([]runtime.Object, error) {
	if owner == nil {
		return nil, errors.New("owner is nil")
	}

	resourceLists, err := cq.discoveryClient.ServerResources()
	if err != nil {
		return nil, err
	}

	var children []runtime.Object

	for resourceListIndex := range resourceLists {
		resourceList := resourceLists[resourceListIndex]
		if resourceList == nil {
			continue
		}

		for i := range resourceList.APIResources {
			apiResource := resourceList.APIResources[i]
			if !apiResource.Namespaced {
				continue
			}

			key := cache.Key{
				Namespace:  owner.GetNamespace(),
				APIVersion: resourceList.GroupVersion,
				Kind:       apiResource.Kind,
			}

			if !containsString("watch", apiResource.Verbs) ||
				!containsString("list", apiResource.Verbs) {
				continue
			}

			objects, err := cq.cache.List(key)
			if err != nil {
				return nil, errors.Wrapf(err, "unable to retrieve %+v", key)
			}

			for _, object := range objects {
				if metav1.IsControlledBy(object, owner) {
					children = append(children, object)
				}
			}
		}
	}

	return children, nil
}

func (cq *CacheQueryer) Events(object metav1.Object) ([]*corev1.Event, error) {
	if object == nil {
		return nil, errors.New("object is nil")
	}

	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}

	u := &unstructured.Unstructured{Object: m}

	key := cache.Key{
		Namespace:  u.GetNamespace(),
		APIVersion: "v1",
		Kind:       "Event",
	}

	allEvents, err := cq.cache.List(key)
	if err != nil {
		return nil, err
	}

	var events []*corev1.Event
	for _, unstructuredEvent := range allEvents {
		event := &corev1.Event{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredEvent.Object, event)
		if err != nil {
			return nil, err
		}

		involvedObject := event.InvolvedObject
		if involvedObject.Namespace == u.GetNamespace() &&
			involvedObject.APIVersion == u.GetAPIVersion() &&
			involvedObject.Kind == u.GetKind() &&
			involvedObject.Name == u.GetName() {
			events = append(events, event)
		}
	}

	return events, nil
}

func (cq *CacheQueryer) IngressesForService(service *corev1.Service) ([]*v1beta1.Ingress, error) {
	if service == nil {
		return nil, errors.New("nil service")
	}

	key := cache.Key{
		Namespace:  service.Namespace,
		APIVersion: "extensions/v1beta1",
		Kind:       "Ingress",
	}
	ul, err := cq.cache.List(key)
	if err != nil {
		return nil, errors.Wrap(err, "retrieving ingresses")
	}

	var results []*v1beta1.Ingress

	for _, u := range ul {
		ingress := &v1beta1.Ingress{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ingress)
		if err != nil {
			return nil, errors.Wrap(err, "converting unstructured ingress")
		}
		if err = copyObjectMeta(ingress, u); err != nil {
			return nil, errors.Wrap(err, "copying object metadata")
		}
		backends := cq.listIngressBackends(*ingress)
		if !containsBackend(backends, service.Name) {
			continue
		}

		results = append(results, ingress)
	}
	return results, nil
}

func (cq *CacheQueryer) listIngressBackends(ingress v1beta1.Ingress) []extv1beta1.IngressBackend {
	var backends []v1beta1.IngressBackend

	if ingress.Spec.Backend != nil && ingress.Spec.Backend.ServiceName != "" {
		backends = append(backends, *ingress.Spec.Backend)
	}

	for _, rule := range ingress.Spec.Rules {
		if rule.IngressRuleValue.HTTP == nil {
			continue
		}
		for _, p := range rule.IngressRuleValue.HTTP.Paths {
			if p.Backend.ServiceName == "" {
				continue
			}
			backends = append(backends, p.Backend)
		}
	}

	return backends
}

func (cq *CacheQueryer) OwnerReference(namespace string, ownerReference metav1.OwnerReference) (runtime.Object, error) {
	key := cache.Key{
		Namespace:  namespace,
		APIVersion: ownerReference.APIVersion,
		Kind:       ownerReference.Kind,
		Name:       ownerReference.Name,
	}

	owner, err := cq.cache.Get(key)
	if err != nil {
		return nil, err
	}

	return owner, nil
}

func (cq *CacheQueryer) PodsForService(service *corev1.Service) ([]*corev1.Pod, error) {
	if service == nil {
		return nil, errors.New("nil service")
	}

	key := cache.Key{
		Namespace:  service.Namespace,
		APIVersion: "v1",
		Kind:       "Pod",
	}

	selector, err := cq.getSelector(service)
	if err != nil {
		return nil, errors.Wrapf(err, "creating pod selector for service: %v", service.Name)
	}
	pods, err := cq.loadPods(key, selector)
	if err != nil {
		return nil, errors.Wrapf(err, "fetching pods for service: %v", service.Name)
	}

	return pods, nil
}

func (cq *CacheQueryer) loadPods(key cache.Key, selector *metav1.LabelSelector) ([]*corev1.Pod, error) {
	objects, err := cq.cache.List(key)
	if err != nil {
		return nil, err
	}

	var list []*corev1.Pod

	for _, object := range objects {
		pod := &corev1.Pod{}
		if err := scheme.Scheme.Convert(object, pod, runtime.InternalGroupVersioner); err != nil {
			return nil, err
		}

		if err := copyObjectMeta(pod, object); err != nil {
			return nil, err
		}

		podSelector := &metav1.LabelSelector{
			MatchLabels: pod.GetLabels(),
		}

		if selector == nil || isEqualSelector(selector, podSelector) {
			list = append(list, pod)
		}
	}

	return list, nil
}

func (cq *CacheQueryer) ServicesForIngress(ingress *extv1beta1.Ingress) ([]*corev1.Service, error) {
	if ingress == nil {
		return nil, errors.New("ingress is nil")
	}

	backends := cq.listIngressBackends(*ingress)
	var services []*corev1.Service
	for _, backend := range backends {
		key := cache.Key{
			Namespace:  ingress.Namespace,
			APIVersion: "v1",
			Kind:       "Service",
			Name:       backend.ServiceName,
		}
		u, err := cq.cache.Get(key)
		if err != nil {
			return nil, errors.Wrapf(err, "retrieving service backend: %v", backend)
		}

		svc := &corev1.Service{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, svc)
		if err != nil {
			return nil, errors.Wrap(err, "converting unstructured service")
		}
		if err := copyObjectMeta(svc, u); err != nil {
			return nil, errors.Wrap(err, "copying object metadata")
		}
		services = append(services, svc)
	}
	return services, nil
}

func (cq *CacheQueryer) ServicesForPod(pod *corev1.Pod) ([]*corev1.Service, error) {
	var results []*corev1.Service
	if pod == nil {
		return nil, errors.New("nil pod")
	}

	key := cache.Key{
		Namespace:  pod.Namespace,
		APIVersion: "v1",
		Kind:       "Service",
	}
	ul, err := cq.cache.List(key)
	if err != nil {
		return nil, errors.Wrap(err, "retrieving services")
	}
	for _, u := range ul {
		svc := &corev1.Service{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, svc)
		if err != nil {
			return nil, errors.Wrap(err, "converting unstructured service")
		}
		if err = copyObjectMeta(svc, u); err != nil {
			return nil, errors.Wrap(err, "copying object metadata")
		}
		labelSelector, err := cq.getSelector(svc)
		if err != nil {
			return nil, errors.Wrapf(err, "creating pod selector for service: %v", svc.Name)
		}
		selector, err := metav1.LabelSelectorAsSelector(labelSelector)
		if err != nil {
			return nil, errors.Wrap(err, "invalid selector")
		}

		if selector.Empty() || !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		results = append(results, svc)
	}
	return results, nil
}

func (cq *CacheQueryer) getSelector(object runtime.Object) (*metav1.LabelSelector, error) {
	switch t := object.(type) {
	case *appsv1.DaemonSet:
		return t.Spec.Selector, nil
	case *appsv1.StatefulSet:
		return t.Spec.Selector, nil
	case *batchv1beta1.CronJob:
		return nil, nil
	case *corev1.ReplicationController:
		selector := &metav1.LabelSelector{
			MatchLabels: t.Spec.Selector,
		}
		return selector, nil
	case *v1beta1.ReplicaSet:
		return t.Spec.Selector, nil
	case *appsv1.ReplicaSet:
		return t.Spec.Selector, nil
	case *appsv1.Deployment:
		return t.Spec.Selector, nil
	case *corev1.Service:
		selector := &metav1.LabelSelector{
			MatchLabels: t.Spec.Selector,
		}
		return selector, nil
	case *apps.DaemonSet:
		return t.Spec.Selector, nil
	case *apps.StatefulSet:
		return t.Spec.Selector, nil
	case *batch.CronJob:
		return nil, nil
	case *core.ReplicationController:
		selector := &metav1.LabelSelector{
			MatchLabels: t.Spec.Selector,
		}
		return selector, nil
	case *apps.ReplicaSet:
		return t.Spec.Selector, nil
	case *apps.Deployment:
		return t.Spec.Selector, nil
	case *core.Service:
		selector := &metav1.LabelSelector{
			MatchLabels: t.Spec.Selector,
		}
		return selector, nil
	default:
		return nil, errors.Errorf("unable to retrieve selector for type %T", object)
	}
}

func copyObjectMeta(to interface{}, from *unstructured.Unstructured) error {
	object, ok := to.(metav1.Object)
	if !ok {
		return errors.Errorf("%T is not an object", to)
	}

	t, err := meta.TypeAccessor(object)
	if err != nil {
		return errors.Wrapf(err, "accessing type meta")
	}
	t.SetAPIVersion(from.GetAPIVersion())
	t.SetKind(from.GetObjectKind().GroupVersionKind().Kind)

	object.SetNamespace(from.GetNamespace())
	object.SetName(from.GetName())
	object.SetGenerateName(from.GetGenerateName())
	object.SetUID(from.GetUID())
	object.SetResourceVersion(from.GetResourceVersion())
	object.SetGeneration(from.GetGeneration())
	object.SetSelfLink(from.GetSelfLink())
	object.SetCreationTimestamp(from.GetCreationTimestamp())
	object.SetDeletionTimestamp(from.GetDeletionTimestamp())
	object.SetDeletionGracePeriodSeconds(from.GetDeletionGracePeriodSeconds())
	object.SetLabels(from.GetLabels())
	object.SetAnnotations(from.GetAnnotations())
	object.SetInitializers(from.GetInitializers())
	object.SetOwnerReferences(from.GetOwnerReferences())
	object.SetClusterName(from.GetClusterName())
	object.SetFinalizers(from.GetFinalizers())

	return nil
}

// extraKeys are keys that should be ignored in labels. These keys are added
// by tools or by Kubernetes itself.
var extraKeys = []string{
	"statefulset.kubernetes.io/pod-name",
	appsv1.DefaultDeploymentUniqueLabelKey,
	"controller-revision-hash",
	"pod-template-generation",
}

func isEqualSelector(s1, s2 *metav1.LabelSelector) bool {
	s1Copy := s1.DeepCopy()
	s2Copy := s2.DeepCopy()

	for _, key := range extraKeys {
		delete(s1Copy.MatchLabels, key)
		delete(s2Copy.MatchLabels, key)
	}

	return apiequality.Semantic.DeepEqual(s1Copy, s2Copy)
}

func containsBackend(lst []v1beta1.IngressBackend, s string) bool {
	for _, item := range lst {
		if item.ServiceName == s {
			return true
		}
	}
	return false
}

func containsString(s string, sl []string) bool {
	for i := range sl {
		if s == sl[i] {
			return true
		}
	}

	return false
}