package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreInformer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	coreLister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	controllerEnabledAnnotation = "com.ingressctrl/enable"
	hostAnnotation              = "com.ingressctrl.host"
	servicePortAnnotation       = "com.ingressctrl.svcport"
	pathAnnotation              = "com.ingressctrl.path"
)

type Event struct {
	key       string
	eventType string
}

type Controller struct {
	kubeclientset kubernetes.Interface
	queue         workqueue.RateLimitingInterface
	serviceLister coreLister.ServiceLister
	serviceSynced cache.InformerSynced
}

func NewController(
	kubeclientset kubernetes.Interface,
	serviceInformer coreInformer.ServiceInformer) *Controller {

	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Serviceq")
	controller := &Controller{
		kubeclientset: kubeclientset,
		queue:         queue,
		serviceLister: serviceInformer.Lister(),
		serviceSynced: serviceInformer.Informer().HasSynced,
	}

	klog.Info("Setting up event handlers")

	var newEvent Event
	var err error
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			newEvent.key, err = cache.MetaNamespaceKeyFunc(obj)
			newEvent.eventType = "create"

			if err == nil {
				queue.Add(newEvent)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSvc := oldObj.(*corev1.Service)
			newSvc := newObj.(*corev1.Service)
			if newSvc.ResourceVersion == oldSvc.ResourceVersion {
				return
			}

			newEvent.key, err = cache.MetaNamespaceKeyFunc(newObj)
			newEvent.eventType = "update"

			if err == nil {
				queue.Add(newEvent)
			}
		},
		DeleteFunc: func(obj interface{}) {
			newEvent.key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			newEvent.eventType = "delete"

			if err == nil {
				queue.Add(newEvent)
			}
		},
	})

	return controller
}

// func (c *Controller) handleObject(obj interface{}) {
// 	var object metav1.Object
// 	var ok bool
// 	if object, ok = obj.(metav1.Object); !ok {
// 		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
// 		if !ok {
// 			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
// 			return
// 		}

// 		object, ok = tombstone.Obj.(metav1.Object)
// 		if !ok {
// 			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
// 			return
// 		}
// 		klog.Infof("Recovered deleted object '%s' from tombstone", object.GetName())
// 	}
// 	klog.Infof("Processing object: %s", object.GetName())
// 	service, err := c.serviceLister.Services(object.GetNamespace()).Get(object.GetName())
// 	if err != nil {
// 		return
// 	}
// 	c.enqueueSvc(service)
// }

// func (c *Controller) enqueueSvc(obj interface{}) {
// 	var key string
// 	var err error
// 	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
// 		utilruntime.HandleError(err)
// 		return
// 	}

// 	c.queue.Add(key)
// }

func (c *Controller) Run(stopch <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Info("Starting the controller %v", c.queue.Len())

	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopch, c.serviceSynced); !ok {
		return fmt.Errorf("failed to wait for cached to sync")
	}

	klog.Info("Starting workers")
	go wait.Until(c.runWorker, time.Second, stopch)

	klog.Info("Started workers")
	<-stopch
	klog.Info("Shutting down workers")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.queue.Get()

	if shutdown {
		return false
	}

	klog.Infof("processing %s", obj.(Event).key)
	err := func(obj interface{}) error {
		defer c.queue.Done(obj)
		var event Event
		var ok bool
		if event, ok = obj.(Event); !ok {
			c.queue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected Event in queue but gor %#v", obj))
			return nil
		}

		if err := c.syncHandler(event); err != nil {
			c.queue.AddRateLimited(obj)
			return fmt.Errorf("error syncing '%s': %s, requeuing", event.key, err.Error())
		}

		c.queue.Forget(obj)
		return nil
	}(obj)

	klog.Infof("done processing %s", obj)
	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) syncHandler(event Event) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(event.key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", name))
		return nil
	}

	// TODO: Handle events that were missed by the controller

	var svc *corev1.Service

	switch event.eventType {
	case "create":
		fallthrough
	case "update":
		svc, err = c.serviceLister.Services(namespace).Get(name)
		if err != nil {
			if errors.IsNotFound(err) {
				utilruntime.HandleError(fmt.Errorf("foo '%s' in work queue no longer exists", name))
				return nil
			}
			return err
		}

		if val, ok := svc.Annotations[controllerEnabledAnnotation]; ok {
			if val == "true" {
				if err := createUpdateIngressRlues(c.kubeclientset, svc); err != nil {
					utilruntime.HandleError(fmt.Errorf("foo '%s' in work queue no longer exists", name))
					return nil
				}
			}
		}

	case "delete":
		svc, err = c.serviceLister.Services(namespace).Get(name)
		if err != nil {
			if errors.IsNotFound(err) {
				if err := deleteIngressRules(c.kubeclientset, namespace, name); err != nil {
					utilruntime.HandleError(fmt.Errorf("foo '%s' in work queue no longer exists", name))
					return nil
				}
				break
			}
			return err
		}

	}

	return nil
}

func createUpdateIngressRlues(clientset kubernetes.Interface, svc *corev1.Service) error {
	ingressName := svc.GetNamespace() + "-ingress"
	ingress, err := getIngress(clientset, svc.GetNamespace(), ingressName)
	if err != nil {
		return err
	}

	var hosts = []string{}
	var paths = []string{}
	var servicePort int32

	fmt.Println(servicePort)
	for key, value := range svc.Annotations {
		annotationKey := strings.Split(key, "/")
		switch annotationKey[0] {
		case hostAnnotation:
			hosts = append(hosts, value)
		case servicePortAnnotation:
			v, _ := strconv.Atoi(value)
			servicePort = int32(v)
		case pathAnnotation:
			paths = append(paths, value)
		}
	}

	var httpIngressPath = []networkv1.HTTPIngressPath{}
	for _, path := range paths {
		pathType := networkv1.PathTypeImplementationSpecific
		ingressPath := networkv1.HTTPIngressPath{
			Path:     path,
			PathType: &pathType,
			Backend: networkv1.IngressBackend{
				Service: &networkv1.IngressServiceBackend{
					Name: svc.Name,
					Port: networkv1.ServiceBackendPort{
						Number: servicePort,
					},
				},
			},
		}
		httpIngressPath = append(httpIngressPath, ingressPath)
	}

	// new ingress
	if len(ingress.Spec.Rules) == 0 {
		for _, host := range hosts {
			ingressRuleValue := networkv1.IngressRuleValue{
				HTTP: &networkv1.HTTPIngressRuleValue{
					Paths: httpIngressPath,
				},
			}
			ingressRule := &networkv1.IngressRule{
				Host:             host,
				IngressRuleValue: ingressRuleValue,
			}

			ingress.Spec.Rules = append(ingress.Spec.Rules, *ingressRule)
		}

		ingress, err = clientset.NetworkingV1().Ingresses(svc.GetNamespace()).Create(context.Background(), ingress, metav1.CreateOptions{})
	} else { // existing ingress
		// if host does not exist add ingressRule
		// if host exists get that reference
		//    if path exists do nothing
		//    if path does not exist add
		existingHosts := map[string]networkv1.IngressRuleValue{}
		for _, rule := range ingress.Spec.Rules {
			existingHosts[rule.Host] = rule.IngressRuleValue
		}

		for _, host := range hosts {
			if iRuleValue, ok := existingHosts[host]; ok {
				existingPaths := map[string]bool{}
				for _, rule := range iRuleValue.HTTP.Paths {
					existingPaths[rule.Path] = true
				}

				for _, path := range paths {
					if _, ok := existingPaths[path]; !ok {
						// Add path
						pathType := networkv1.PathTypeImplementationSpecific
						ingressPath := networkv1.HTTPIngressPath{
							Path:     path,
							PathType: &pathType,
							Backend: networkv1.IngressBackend{
								Service: &networkv1.IngressServiceBackend{
									Name: svc.Name,
									Port: networkv1.ServiceBackendPort{
										Number: servicePort,
									},
								},
							},
						}
						iRuleValue.HTTP.Paths = append(iRuleValue.HTTP.Paths, ingressPath)
					}
				}
			} else {
				// if host does not exists then add rule
				ingressRuleValue := networkv1.IngressRuleValue{
					HTTP: &networkv1.HTTPIngressRuleValue{
						Paths: httpIngressPath,
					},
				}
				ingressRule := &networkv1.IngressRule{
					Host:             host,
					IngressRuleValue: ingressRuleValue,
				}

				ingress.Spec.Rules = append(ingress.Spec.Rules, *ingressRule)
			}
		}
		ingress, err = clientset.NetworkingV1().Ingresses(svc.GetNamespace()).Update(context.Background(), ingress, metav1.UpdateOptions{})
	}
	return nil
}

func getIngress(clientset kubernetes.Interface, namespace string, ingressName string) (*networkv1.Ingress, error) {
	var ingress *networkv1.Ingress
	var err error

	ingress, err = clientset.NetworkingV1().Ingresses(namespace).Get(context.Background(), ingressName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			ingress = createIngress(namespace, ingressName)
		} else {
			return nil, err
		}
	}
	return ingress, nil
}

func createIngress(namespace, ingressName string) *networkv1.Ingress {
	return &networkv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressName,
			Namespace: namespace,
		},
		Spec: networkv1.IngressSpec{},
	}
}

func deleteIngressRules(clientset kubernetes.Interface, namespace string, name string) error {
	// if service is deleted
	// delete by name:
	//   find all paths where backend matches
	//      delete the path
	// if host has no paths delete host

	ingress, err := clientset.NetworkingV1().Ingresses(namespace).Get(context.Background(), namespace+"-ingress", metav1.GetOptions{})
	if err != nil {
		return err
	}

	newHosts := []networkv1.IngressRule{}
	for _, host := range ingress.Spec.Rules {
		newPaths := []networkv1.HTTPIngressPath{}
		for _, path := range host.HTTP.Paths {
			if path.Backend.Service.Name != name {
				newPaths = append(newPaths, path)
			}
		}

		host.HTTP.Paths = newPaths
		if len(host.HTTP.Paths) != 0 {
			newHosts = append(newHosts, host)
		}
	}

	ingress.Spec.Rules = newHosts
	if len(ingress.Spec.Rules) == 0 {
		err = clientset.NetworkingV1().Ingresses(namespace).Delete(context.Background(), namespace+"-ingress", metav1.DeleteOptions{})
	} else {
		ingress, err = clientset.NetworkingV1().Ingresses(namespace).Update(context.Background(), ingress, metav1.UpdateOptions{})
	}
	return nil
}
