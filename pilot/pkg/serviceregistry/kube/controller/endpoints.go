// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"fmt"
	"reflect"

	v1 "k8s.io/api/core/v1"
	discoveryv1alpha1 "k8s.io/api/discovery/v1alpha1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	configKube "istio.io/istio/pkg/config/kube"
	"istio.io/istio/pkg/config/labels"
	"istio.io/pkg/log"
)

// Pilot can get EDS information from Kubernetes from two mutually exclusive sources, Endpoints and
// EndpointSlices. The edsController abstracts these details and provides a common interface that
// both sources implement
type edsController interface {
	Get() cacheHandler
	AppendInstanceHandler(c *Controller)
	InstancesByPort(c *Controller, svc *model.Service, reqSvcPort int,
		labelsList labels.Collection) ([]*model.ServiceInstance, error)
	GetEndpointServiceInstances(c *Controller, proxy *model.Proxy, proxyNamespace string) []*model.ServiceInstance
}

type endpointsController struct {
	cache cacheHandler
}

var _ edsController = &endpointsController{}

func NewEndpointsController(c *Controller, sharedInformers informers.SharedInformerFactory) *endpointsController {
	epInformer := sharedInformers.Core().V1().Endpoints().Informer()
	return &endpointsController{createEDSCacheHandler(c, epInformer, "Endpoints")}
}

func (e *endpointsController) GetEndpointServiceInstances(c *Controller, proxy *model.Proxy, proxyNamespace string) []*model.ServiceInstance {
	endpointsForPodInSameNS := make([]*model.ServiceInstance, 0)
	endpointsForPodInDifferentNS := make([]*model.ServiceInstance, 0)

	for _, item := range e.cache.informer.GetStore().List() {
		ep := *item.(*v1.Endpoints)
		endpoints := &endpointsForPodInSameNS
		if ep.Namespace != proxyNamespace {
			endpoints = &endpointsForPodInDifferentNS
		}

		*endpoints = append(*endpoints, c.getProxyServiceInstancesByEndpoint(ep, proxy)...)
	}

	// Put the endpointsForPodInSameNS in front of endpointsForPodInDifferentNS so that Pilot will
	// first use endpoints from endpointsForPodInSameNS. This makes sure if there are two endpoints
	// referring to the same IP/port, the one in endpointsForPodInSameNS will be used. (The other one
	// in endpointsForPodInDifferentNS will thus be rejected by Pilot).
	return append(endpointsForPodInSameNS, endpointsForPodInDifferentNS...)
}

func (e *endpointsController) InstancesByPort(c *Controller, svc *model.Service, reqSvcPort int,
	labelsList labels.Collection) ([]*model.ServiceInstance, error) {
	item, exists, err := e.cache.informer.GetStore().GetByKey(kube.KeyFunc(svc.Attributes.Name, svc.Attributes.Namespace))
	if err != nil {
		log.Infof("get endpoints(%s, %s) => error %v", svc.Attributes.Name, svc.Attributes.Namespace, err)
		return nil, nil
	}
	if !exists {
		return nil, nil
	}

	mixerEnabled := c.Env != nil && c.Env.Mesh != nil && (c.Env.Mesh.MixerCheckServer != "" || c.Env.Mesh.MixerReportServer != "")
	// Locate all ports in the actual service
	svcPortEntry, exists := svc.Ports.GetByPort(reqSvcPort)
	if !exists {
		return nil, nil
	}
	ep := item.(*v1.Endpoints)
	var out []*model.ServiceInstance
	for _, ss := range ep.Subsets {
		for _, ea := range ss.Addresses {
			var podLabels labels.Instance
			pod := c.pods.getPodByIP(ea.IP)
			if pod != nil {
				podLabels = configKube.ConvertLabels(pod.ObjectMeta)
			}
			// check that one of the input labels is a subset of the labels
			if !labelsList.HasSubsetOf(podLabels) {
				continue
			}

			az, sa, uid := "", "", ""
			if pod != nil {
				az = c.GetPodLocality(pod)
				sa = kube.SecureNamingSAN(pod)
				if mixerEnabled {
					uid = fmt.Sprintf("kubernetes://%s.%s", pod.Name, pod.Namespace)
				}
			}
			mtlsReady := kube.PodMTLSReady(pod)

			// identify the port by name. K8S EndpointPort uses the service port name
			for _, port := range ss.Ports {
				if port.Name == "" || // 'name optional if single port is defined'
					svcPortEntry.Name == port.Name {
					out = append(out, &model.ServiceInstance{
						Endpoint: model.NetworkEndpoint{
							Address:     ea.IP,
							Port:        int(port.Port),
							ServicePort: svcPortEntry,
							UID:         uid,
							Network:     c.endpointNetwork(ea.IP),
							Locality:    az,
						},
						Service:        svc,
						Labels:         podLabels,
						ServiceAccount: sa,
						MTLSReady:      mtlsReady,
					})
				}
			}
		}
	}

	return out, nil
}

func (e *endpointsController) AppendInstanceHandler(c *Controller) {
	if e.cache.handler == nil {
		return
	}
	e.cache.handler.Append(func(obj interface{}, event model.Event) error {
		ep, ok := obj.(*v1.Endpoints)
		if !ok {
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if !ok {
				log.Errorf("Couldn't get object from tombstone %#v", obj)
				return nil
			}
			ep, ok = tombstone.Obj.(*v1.Endpoints)
			if !ok {
				log.Errorf("Tombstone contained object that is not an endpoints %#v", obj)
				return nil
			}
		}

		c.updateEDS(ep, event)

		return nil
	})
}

func (e *endpointsController) Get() cacheHandler {
	return e.cache
}

func createEDSCacheHandler(c *Controller, informer cache.SharedIndexInformer, otype string) cacheHandler {
	handler := &kube.ChainHandler{Funcs: []kube.Handler{c.notify}}

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			// TODO: filtering functions to skip over un-referenced resources (perf)
			AddFunc: func(obj interface{}) {
				incrementEvent(otype, "add")
				c.queue.Push(kube.Task{Handler: handler.Apply, Obj: obj, Event: model.EventAdd})
			},
			UpdateFunc: func(old, cur interface{}) {
				// Avoid pushes if only resource version changed (kube-scheduller, cluster-autoscaller, etc)
				oldE := old.(*v1.Endpoints)
				curE := cur.(*v1.Endpoints)

				if !reflect.DeepEqual(oldE.Subsets, curE.Subsets) {
					incrementEvent(otype, "update")
					c.queue.Push(kube.Task{Handler: handler.Apply, Obj: cur, Event: model.EventUpdate})
				} else {
					incrementEvent(otype, "updatesame")
				}
			},
			DeleteFunc: func(obj interface{}) {
				incrementEvent(otype, "delete")
				// Deleting the endpoints results in an empty set from EDS perspective - only
				// deleting the service should delete the resources. The full sync replaces the
				// maps.
				// c.updateEDS(obj.(*v1.Endpoints))
				c.queue.Push(kube.Task{Handler: handler.Apply, Obj: obj, Event: model.EventDelete})
			},
		})

	return cacheHandler{informer: informer, handler: handler}
}

type endpointSliceController struct {
	cache cacheHandler
}

var _ edsController = &endpointSliceController{}

func NewEndpointSliceController(c *Controller, sharedInformers informers.SharedInformerFactory) *endpointSliceController {
	epSliceInformer := sharedInformers.Discovery().V1alpha1().EndpointSlices().Informer()
	// TODO Endpoints has a special cache, to filter out irrelevant updates to kube-system
	// Investigate if we need this, or if EndpointSlice is makes this not relevant
	return &endpointSliceController{c.createCacheHandler(epSliceInformer, "EndpointSlice")}
}

func (e endpointSliceController) Get() cacheHandler {
	return e.cache
}

func (e endpointSliceController) AppendInstanceHandler(c *Controller) {
	if e.cache.handler == nil {
		return
	}
	e.cache.handler.Append(func(obj interface{}, event model.Event) error {
		ep, ok := obj.(*discoveryv1alpha1.EndpointSlice)
		if !ok {
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if !ok {
				log.Errorf("Couldn't get object from tombstone %#v", obj)
				return nil
			}
			ep, ok = tombstone.Obj.(*discoveryv1alpha1.EndpointSlice)
			if !ok {
				log.Errorf("Tombstone contained an object that is not an endpoints slice %#v", obj)
				return nil
			}
		}

		c.updateEDSSlice(ep, event)

		return nil
	})
}

func (e endpointSliceController) GetEndpointServiceInstances(c *Controller, proxy *model.Proxy, proxyNamespace string) []*model.ServiceInstance {
	endpointsForPodInSameNS := make([]*model.ServiceInstance, 0)
	endpointsForPodInDifferentNS := make([]*model.ServiceInstance, 0)

	for _, item := range e.cache.informer.GetStore().List() {
		slice := *item.(*discoveryv1alpha1.EndpointSlice)
		endpoints := &endpointsForPodInSameNS
		if slice.Namespace != proxyNamespace {
			endpoints = &endpointsForPodInDifferentNS
		}

		*endpoints = append(*endpoints, c.getProxyServiceInstancesByEndpointSlice(slice, proxy)...)
	}

	// Put the endpointsForPodInSameNS in front of endpointsForPodInDifferentNS so that Pilot will
	// first use endpoints from endpointsForPodInSameNS. This makes sure if there are two endpoints
	// referring to the same IP/port, the one in endpointsForPodInSameNS will be used. (The other one
	// in endpointsForPodInDifferentNS will thus be rejected by Pilot).
	return append(endpointsForPodInSameNS, endpointsForPodInDifferentNS...)
}

func (e *endpointSliceController) InstancesByPort(c *Controller, svc *model.Service, reqSvcPort int,
	labelsList labels.Collection) ([]*model.ServiceInstance, error) {
	esLabelSelector := klabels.Set(map[string]string{discoveryv1alpha1.LabelServiceName: svc.Attributes.Name}).AsSelectorPreValidated()
	var slices []*discoveryv1alpha1.EndpointSlice
	err := cache.ListAllByNamespace(e.cache.informer.GetIndexer(), svc.Attributes.Namespace, esLabelSelector, func(i interface{}) {
		slices = append(slices, i.(*discoveryv1alpha1.EndpointSlice))
	})
	if err != nil {
		log.Infof("get endpoints(%s, %s) => error %v", svc.Attributes.Name, svc.Attributes.Namespace, err)
		return nil, nil
	}
	if len(slices) == 0 {
		return nil, nil
	}

	mixerEnabled := c.Env != nil && c.Env.Mesh != nil && (c.Env.Mesh.MixerCheckServer != "" || c.Env.Mesh.MixerReportServer != "")
	// Locate all ports in the actual service
	svcPortEntry, exists := svc.Ports.GetByPort(reqSvcPort)
	if !exists {
		return nil, nil
	}

	var out []*model.ServiceInstance
	for _, slice := range slices {
		for _, e := range slice.Endpoints {
			for _, a := range e.Addresses {
				var podLabels labels.Instance
				pod := c.pods.getPodByIP(a)
				if pod != nil {
					podLabels = configKube.ConvertLabels(pod.ObjectMeta)
				}
				// check that one of the input labels is a subset of the labels
				if !labelsList.HasSubsetOf(podLabels) {
					continue
				}

				az, sa, uid := "", "", ""
				if pod != nil {
					az = c.GetPodLocality(pod)
					sa = kube.SecureNamingSAN(pod)
					if mixerEnabled {
						uid = fmt.Sprintf("kubernetes://%s.%s", pod.Name, pod.Namespace)
					}
				}
				mtlsReady := kube.PodMTLSReady(pod)

				// identify the port by name. K8S EndpointPort uses the service port name
				for _, port := range slice.Ports {
					var portNum uint32
					if port.Port != nil {
						portNum = uint32(*port.Port)
					}

					if port.Name == nil ||
						svcPortEntry.Name == *port.Name {

						out = append(out, &model.ServiceInstance{
							Endpoint: model.NetworkEndpoint{
								Address:     a,
								Port:        int(portNum),
								ServicePort: svcPortEntry,
								UID:         uid,
								Network:     c.endpointNetwork(a),
								Locality:    az,
							},
							Service:        svc,
							Labels:         podLabels,
							ServiceAccount: sa,
							MTLSReady:      mtlsReady,
						})
					}
				}
			}
		}
	}
	return out, nil
}
