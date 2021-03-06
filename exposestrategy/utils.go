package exposestrategy

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"text/template"

	"github.com/pkg/errors"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

func findHTTPProtocol(svc *v1.Service, hostName string) string {
	// default to http
	protocol := "http"

	// if a port is on the hostname check is its a default http / https port
	_, port, err := net.SplitHostPort(hostName)
	if err == nil {
		if port == "443" || port == "8443" {
			protocol = "https"
		}
	}
	// check if the service port has a name of https
	for _, port := range svc.Spec.Ports {
		if port.Name == "https" {
			protocol = port.Name
		}
	}
	return protocol
}

func addServiceAnnotation(svc *v1.Service, hostName string) error {
	protocol := findHTTPProtocol(svc, hostName)
	return addServiceAnnotationWithProtocol(svc, hostName, "", protocol)
}

func addServiceAnnotationWithProtocol(svc *v1.Service, hostName, path, protocol string) error {
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	if hostName == "" {
		svc.Annotations[ExposeAnnotationKey] = ""
		return nil
	}

	exposeURL := protocol + "://" + hostName
	if annotationPath, ok := svc.Annotations[APIServicePathAnnotationKey]; ok {
		path = annotationPath
	}
	if len(path) > 0 {
		exposeURL = urlJoin(exposeURL, path)
	}
	svc.Annotations[ExposeAnnotationKey] = exposeURL

	if key := svc.Annotations[ExposeHostNameAsAnnotationKey]; key != "" {
		svc.Annotations[key] = hostName
	}

	return nil
}

func removeServiceAnnotation(svc *v1.Service) bool {
	if _, ok := svc.Annotations[ExposeAnnotationKey]; !ok {
		return false
	}
	delete(svc.Annotations, ExposeAnnotationKey)
	if key := svc.Annotations[ExposeHostNameAsAnnotationKey]; key != "" {
		delete(svc.Annotations, key)
	}
	return true
}

// urlJoin joins the given URL paths so that there is a / separating them but not a double //
func urlJoin(repo string, path string) string {
	return strings.TrimSuffix(repo, "/") + "/" + strings.TrimPrefix(path, "/")
}

var patchType types.PatchType = types.StrategicMergePatchType
var emptyPatch []byte = []byte("{}")

func createServicePatch(origin, modified *v1.Service) ([]byte, error) {
	// add another annotations to avoid a patch that deletes all annotations
	copy := origin.DeepCopy()
	if copy.Annotations == nil {
		copy.Annotations = map[string]string{"#": "#"}
	} else {
		copy.Annotations["#"] = "#"
	}
	if modified.Annotations == nil {
		modified.Annotations = map[string]string{"#": "#"}
	} else {
		modified.Annotations["#"] = "#"
	}

	originBytes, err := json.Marshal(copy)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to encode object: %v", copy)
	}
	modifiedBytes, err := json.Marshal(modified)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to encode object: %v", modified)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(originBytes, modifiedBytes, &v1.Service{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create patch")
	}

	if bytes.Equal(patch, emptyPatch) {
		return nil, nil
	}
	return patch, nil
}

type urlTemplateParts struct {
	Service   string
	Namespace string
	Domain    string
}

func getURLFormat(urltemplate string) (string, error) {
	if urltemplate == "" {
		urltemplate = "{{.Service}}.{{.Namespace}}.{{.Domain}}"
	}
	placeholders := urlTemplateParts{"%[1]s", "%[2]s", "%[3]s"}
	tmpl, err := template.New("format").Parse(urltemplate)
	if err != nil {
		errors.Wrap(err, "Failed to parse URLTemplate")
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, placeholders)
	if err != nil {
		errors.Wrap(err, "Failed to execute URLTemplate")
	}
	return buffer.String(), nil
}

// URLJoin joins the given paths so that there is only ever one '/' character between the paths
func URLJoin(paths ...string) string {
	var buffer bytes.Buffer
	last := len(paths) - 1
	for i, path := range paths {
		p := path
		if i > 0 {
			buffer.WriteString("/")
			p = strings.TrimPrefix(p, "/")
		}
		if i < last {
			p = strings.TrimSuffix(p, "/")
		}
		buffer.WriteString(p)
	}
	return buffer.String()
}
