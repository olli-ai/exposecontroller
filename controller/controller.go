package controller

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/watch"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	uapi "k8s.io/kubernetes/pkg/api/unversioned"

	"github.com/fabric8io/exposecontroller/exposestrategy"

	oclient "github.com/openshift/origin/pkg/client"
	oauthapi "github.com/openshift/origin/pkg/oauth/api"
	oauthapiv1 "github.com/openshift/origin/pkg/oauth/api/v1"
	"net/url"
)

const (
	ExposeURLKeyAnnotation = "expose-url.fabric8.io/url-key"
	ExposeURLHostKeyAnnotation = "expose-url.fabric8.io/host-key"
)

type Controller struct {
	client *client.Client

	svcController *framework.Controller
	svcLister     cache.StoreToServiceLister

	config *Config

	recorder record.EventRecorder

	stopCh chan struct{}
}

func NewController(
	kubeClient *client.Client,
	restClientConfig *restclient.Config,
	encoder runtime.Encoder,
	resyncPeriod time.Duration, namespace string, config *Config) (*Controller, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(namespace))

	c := Controller{
		client: kubeClient,
		stopCh: make(chan struct{}),
		config: config,
		recorder: eventBroadcaster.NewRecorder(api.EventSource{
			Component: "expose-controller",
		}),
	}

	strategy, err := exposestrategy.New(config.Exposer, config.Domain, kubeClient, restClientConfig, encoder)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new strategy")
	}

	var oc *oclient.Client = nil
	if isOpenShift(kubeClient) {
		// register openshift schemas
		oauthapi.AddToScheme(api.Scheme)
		oauthapiv1.AddToScheme(api.Scheme)

		ocfg := *restClientConfig
		ocfg.APIPath = ""
		ocfg.GroupVersion = nil
		ocfg.NegotiatedSerializer = nil
		oc, _ = oclient.New(&ocfg)
	}

	c.svcLister.Store, c.svcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  serviceListFunc(c.client, namespace),
			WatchFunc: serviceWatchFunc(c.client, namespace),
		},
		&api.Service{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				svc := obj.(*api.Service)
				if svc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value {
					err := strategy.Add(svc)
					if err != nil {
						glog.Errorf("Add failed: %v", err)
					}
					updateRelatedResources(kubeClient, oc, svc)
				}
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				svc := newObj.(*api.Service)
				if svc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value {
					err := strategy.Add(svc)
					if err != nil {
						glog.Errorf("Add failed: %v", err)
					}
					updateRelatedResources(kubeClient, oc, svc)
				} else {
					oldSvc := oldObj.(*api.Service)
					if oldSvc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value {
						err := strategy.Remove(svc)
						if err != nil {
							glog.Errorf("Remove failed: %v", err)
						}
						updateRelatedResources(kubeClient, oc, svc)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				svc, ok := obj.(cache.DeletedFinalStateUnknown)
				if ok {
					// service key is in the form namespace/name
					split := strings.Split(svc.Key, "/")
					ns := split[0]
					name := split[1]
					err := strategy.Remove(&api.Service{ObjectMeta: api.ObjectMeta{Namespace: ns, Name: name}})
					if err != nil {
						glog.Errorf("Remove failed: %v", err)
					}
				}
			},
		},
	)

	return &c, nil
}


func isOpenShift(c *client.Client) bool {
	res, err := c.Get().AbsPath("").DoRaw()
	if err != nil {
		glog.Errorf("Could not discover the type of your installation: %v", err)
		return false
	}

	var rp uapi.RootPaths
	err = json.Unmarshal(res, &rp)
	if err != nil {
		glog.Errorf("Could not discover the type of your installation: %v", err)
		return false
	}
	for _, p := range rp.Paths {
		if p == "/oapi" {
			return true
		}
	}
	return false
}

func updateRelatedResources(c *client.Client, oc *oclient.Client, svc *api.Service) {
	updateServiceConfigMap(c, svc)
	if oc != nil {
		updateServiceOAuthClient(oc, svc)
	}
}


func updateServiceConfigMap(c *client.Client, svc *api.Service) {
	name := svc.Name
	ns := svc.Namespace
	exposeURL := svc.Annotations[exposestrategy.ExposeAnnotationKey]
	if len(exposeURL) > 0 {
		host := ""
		cm, err := c.ConfigMaps(ns).Get(name)
		if err == nil {
			url, err := url.Parse(exposeURL)
			if err != nil {
				glog.Errorf("Failed to parse expose URL %s for service %s  error: %v", exposeURL, name, err)

			} else {
				host = url.Host
			}
			updated := false
			urlKey := cm.Annotations[ExposeURLKeyAnnotation]
			domainKey := cm.Annotations[ExposeURLHostKeyAnnotation]
			if len(urlKey) > 0 {
				if cm.Data[urlKey] != exposeURL {
					cm.Data[urlKey] = exposeURL
					updated = true
				}
			}
			if len(host) > 0 && len(domainKey) > 0 {
				if cm.Data[domainKey] != host {
					cm.Data[domainKey] = host
					updated = true
				}
			}
			if updated {
				glog.Infof("Updating ConfigMap %s/%s", ns, name)
				_, err = c.ConfigMaps(ns).Update(cm)
				if err != nil {
					glog.Errorf("Failed to update ConfigMap %s error: %v", name, err)
				}
			}
		}
	}
}

func updateServiceOAuthClient(oc *oclient.Client, svc *api.Service) {
	name := svc.Name
	exposeURL := svc.Annotations[exposestrategy.ExposeAnnotationKey]
	if len(exposeURL) > 0 {
		oauthClient, err := oc.OAuthClients().Get(name)
		if err == nil {
			redirects := oauthClient.RedirectURIs
			found := false
			for _, uri := range redirects {
				if uri == exposeURL {
					found = true
					break
				}
			}
			if !found {
				oauthClient.RedirectURIs = append(redirects, exposeURL)
				glog.Infof("Deleting OAuthClient %s", name)
				err = oc.OAuthClients().Delete(name)
				if err != nil {
					glog.Errorf("Failed to delete OAuthClient %s error: %v", name, err)
					return
				}
				oauthClient.ResourceVersion = ""
				glog.Infof("Creating OAuthClient %s with redirectURIs %v", name, oauthClient.RedirectURIs)
				_, err = oc.OAuthClients().Create(oauthClient)
				if err != nil {
					glog.Errorf("Failed to delete OAuthClient %s error: %v", name, err)
					return
				}
			}
		}
	}
}


// Run starts the controller.
func (c *Controller) Run() {
	glog.Infof("starting expose controller")

	go c.svcController.Run(c.stopCh)

	<-c.stopCh
}

func (c *Controller) Stop() {
	glog.Infof("stopping expose controller")

	close(c.stopCh)
}

func serviceListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Services(ns).List(opts)
	}
}

func serviceWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Services(ns).Watch(options)
	}
}