package exposestrategy

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// const (
// 	PathModeUsePath = "path"
// )

type AmbassadorStrategy struct {
	client  kubernetes.Interface

	domain        string
	tlsSecretName string
	http          bool
	tlsAcme       bool
	urltemplate   string
	pathMode      string
}

func NewAmbassadorStrategy(client kubernetes.Interface, config *ExposeStrategyConfig) (ExposeStrategy, error) {

	var err error
	if len(config.Domain) == 0 {
		config.Domain, err = getAutoDefaultDomain(client)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get a domain")
		}
	}
	glog.Infof("Using domain: %s", config.Domain)

	var urlformat string
	urlformat, err = getURLFormat(config.URLTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get a url format")
	}
	glog.Infof("Using url template [%s] format [%s]", config.URLTemplate, urlformat)

	return &AmbassadorStrategy{
		client:        client,
		domain:        config.Domain,
		http:          config.HTTP,
		tlsAcme:       config.TLSAcme,
		tlsSecretName: config.TLSSecretName,
		urltemplate:   urlformat,
		pathMode:      config.PathMode,
	}, nil
}

func (s *AmbassadorStrategy) Sync() error {
	return nil
}

func (s *AmbassadorStrategy) Add(svc *v1.Service) error {
	appName := svc.Annotations["fabric8.io/ingress.name"]
	if appName == "" {
		if svc.Labels["release"] != "" {
			appName = strings.Replace(svc.Name, svc.Labels["release"]+"-", "", 1)
		} else {
			appName = svc.Name
		}
	}

	hostName := svc.Annotations["fabric8.io/host.name"]
	if hostName == "" {
		hostName = appName
	}

	hostName = fmt.Sprintf(s.urltemplate, hostName, svc.Namespace, s.domain)
	// fullHostName := hostName
	path := svc.Annotations["fabric8.io/ingress.path"]
	pathMode := svc.Annotations["fabric8.io/path.mode"]
	if pathMode == "" {
		pathMode = s.pathMode
	}
	if pathMode == PathModeUsePath {
		suffix := path
		if len(suffix) == 0 {
			suffix = "/"
		}
		path = UrlJoin("/", svc.Namespace, appName, suffix)
		hostName = s.domain
		// fullHostName = UrlJoin(hostName, path)
	}

	exposePort := svc.Annotations[ExposePortAnnotationKey]
	if exposePort != "" {
		port, err := strconv.Atoi(exposePort)
		if err == nil {
			found := false
			for _, p := range svc.Spec.Ports {
				if port == int(p.Port) {
					found = true
					break
				}
			}
			if !found {
				glog.Warningf("Port '%s' provided in the annotation '%s' is not available in the ports of service '%s'",
					exposePort, ExposePortAnnotationKey, svc.GetName())
				exposePort = ""
			}
		} else {
			glog.Warningf("Port '%s' provided in the annotation '%s' is not a valid number",
				exposePort, ExposePortAnnotationKey)
			exposePort = ""
		}
	}
	// Pick the fist port available in the service if no expose port was configured
	if exposePort == "" {
		port := svc.Spec.Ports[0]
		exposePort = strconv.Itoa(int(port.Port))
	}

	servicePort, err := strconv.Atoi(exposePort)
	if err != nil {
		return errors.Wrapf(err, "failed to convert the exposed port '%s' to int", exposePort)
	}
	glog.Infof("Exposing Port %d of Service %s", servicePort, svc.Name)

	// Here's where we start adding the annotations to our service
	ambassadorAnnotations := map[string]interface{}{
		"apiVersion": "ambassador/v1",
		"kind":       "Mapping",
		"host":       hostName,
		"name":       fmt.Sprintf("%s_%s_mapping", hostName, svc.Namespace),
		"service":    fmt.Sprintf("%s.%s:%s", appName, svc.Namespace, strconv.Itoa(servicePort))}

	joinedAnnotations := new(bytes.Buffer)
	fmt.Fprintf(joinedAnnotations, "---\n")
	yamlAnnotation, err := yaml.Marshal(&ambassadorAnnotations)
	if err != nil {
		return err
	}
	fmt.Fprintf(joinedAnnotations, "%s", string(yamlAnnotation))

	if s.tlsAcme && s.tlsSecretName == "" {
		s.tlsSecretName = "tls-" + appName
	}

	if s.isTLSEnabled(svc) {
		// we need to prepare the tls module config
		ambassadorAnnotations = map[string]interface{}{
			"apiVersion": "ambassador/v1",
			"kind":       "Module",
			"name":       "tls",
			"config": map[string]interface{}{
				"server": map[string]interface{}{
					"enabled": "True",
					"secret":  s.tlsSecretName}}}

		yamlAnnotation, err = yaml.Marshal(&ambassadorAnnotations)
		if err != nil {
			return err
		}

		fmt.Fprintf(joinedAnnotations, "---\n")
		fmt.Fprintf(joinedAnnotations, "%s", string(yamlAnnotation))
	}

	clone := svc.DeepCopy()
	clone.Annotations["getambassador.io/config"] = joinedAnnotations.String()

	patch, err := createServicePatch(svc, clone)
	if err != nil {
		return errors.Wrapf(err, "failed to create patch for service %s/%s",
			svc.Namespace, svc.Name)
	}
	// patch the service
	if patch != nil {
		_, err = s.client.CoreV1().Services(svc.Namespace).
			Patch(svc.Name, patchType, patch)
		if err != nil {
			return errors.Wrapf(err, "failed to send patch %s/%s",
				svc.Namespace, svc.Name)
		}
	}

	return nil
}

func (s *AmbassadorStrategy) Remove(svc *v1.Service) error {
	clone := svc.DeepCopy()
	delete(svc.Annotations, "getambassador.io/config")

	patch, err := createServicePatch(svc, clone)
	if err != nil {
		return errors.Wrapf(err, "failed to create patch for service %s/%s",
			svc.Namespace, svc.Name)
	}
	// patch the service
	if patch != nil {
		_, err = s.client.CoreV1().Services(svc.Namespace).
			Patch(svc.Name, patchType, patch)
		if err != nil {
			return errors.Wrapf(err, "failed to send patch %s/%s",
				svc.Namespace, svc.Name)
		}
	}
	return nil
}

func (s *AmbassadorStrategy) isTLSEnabled(svc *v1.Service) bool {
	if svc != nil && svc.Annotations["jenkins-x.io/skip.tls"] == "true" {
		return false
	}

	if len(s.tlsSecretName) > 0 || s.tlsAcme {
		return true
	}

	return false
}
