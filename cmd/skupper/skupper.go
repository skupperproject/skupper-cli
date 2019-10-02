package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
        "k8s.io/apimachinery/pkg/runtime/serializer"
        "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
        "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/spf13/cobra"
	"github.com/skupperproject/skupper-cli/pkg/certs"
	"github.com/skupperproject/skupper-cli/pkg/router"
	"github.com/skupperproject/skupper-cli/pkg/kube"
)

var version = "undefined"

type RouterMode string

const (
	RouterModeInterior RouterMode = "interior"
	RouterModeEdge                = "edge"
)

type ConnectorRole string

const (
	ConnectorRoleInterRouter ConnectorRole = "inter-router"
	ConnectorRoleEdge                      = "edge"
)

func connectJson() string {
	connect_json := `
{
    "scheme": "amqps",
    "host": "skupper-messaging",
    "port": "5671",
    "tls": {
        "ca": "/etc/messaging/ca.crt",
        "cert": "/etc/messaging/tls.crt",
        "key": "/etc/messaging/tls.key",
        "verify": true
    }
}
`
	return connect_json
}

type ConsoleAuthMode string

const (
	ConsoleAuthModeOpenshift ConsoleAuthMode = "openshift"
	ConsoleAuthModeInternal                  = "internal"
	ConsoleAuthModeUnsecured                 = "unsecured"
)

type Router struct {
	Name string
	Mode RouterMode
	Replicas int32
	Console ConsoleAuthMode
	ConsoleUser string
	ConsolePassword string
}

func routerConfig(router *Router) string {
	config := `
router {
    mode: {{.Mode}}
    id: {{.Name}}-${HOSTNAME}
}

listener {
    host: localhost
    port: 5672
    role: normal
}

sslProfile {
    name: skupper-amqps
    certFile: /etc/qpid-dispatch-certs/skupper-amqps/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/skupper-amqps/tls.key
    caCertFile: /etc/qpid-dispatch-certs/skupper-amqps/ca.crt
}

listener {
    host: 0.0.0.0
    port: 5671
    role: normal
    sslProfile: skupper-amqps
    saslMechanisms: EXTERNAL
    authenticatePeer: true
}

{{- if eq .Console "openshift"}}
# console secured by oauth proxy sidecar
listener {
    host: localhost
    port: 8888
    role: normal
    http: true
}
{{- else if eq .Console "internal"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
    authenticatePeer: true
}
{{- else if eq .Console "unsecured"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
}
{{- end }}

listener {
    host: 0.0.0.0
    port: 9090
    role: normal
    http: true
    httpRootDir: disabled
    websockets: false
    healthz: true
    metrics: true
}

{{- if eq .Mode "interior" }}
sslProfile {
    name: skupper-internal
    certFile: /etc/qpid-dispatch-certs/skupper-internal/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/skupper-internal/tls.key
    caCertFile: /etc/qpid-dispatch-certs/skupper-internal/ca.crt
}

listener {
    role: inter-router
    host: 0.0.0.0
    port: 55671
    sslProfile: skupper-internal
    saslMechanisms: EXTERNAL
    authenticatePeer: true
}

listener {
    role: edge
    host: 0.0.0.0
    port: 45671
    sslProfile: skupper-internal
    saslMechanisms: EXTERNAL
    authenticatePeer: true
}
{{- end}}

address {
    prefix: mc
    distribution: multicast
}

## Connectors: ##
`
	var buff bytes.Buffer
	qdrconfig := template.Must(template.New("qdrconfig").Parse(config))
	qdrconfig.Execute(&buff, router)
	return buff.String()
}

type Connector struct {
	Name string
	Host string
	Port string
	Role ConnectorRole
}

func connectorConfig(connector *Connector) string {
	config := `

sslProfile {
    name: {{.Name}}-profile
    certFile: /etc/qpid-dispatch-certs/{{.Name}}/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/{{.Name}}/tls.key
    caCertFile: /etc/qpid-dispatch-certs/{{.Name}}/ca.crt
}

connector {
    name: {{.Name}}-connector
    host: {{.Host}}
    port: {{.Port}}
    role: {{.Role}}
    sslProfile: {{.Name}}-profile
}

`
	var buff bytes.Buffer
	connectorconfig := template.Must(template.New("connectorconfig").Parse(config))
	connectorconfig.Execute(&buff, connector)
	return buff.String()
}

func mountConfigVolume(name string, path string, containerIndex int, router *appsv1.Deployment) {
	//define volume in deployment
	volumes := router.Spec.Template.Spec.Volumes
	if volumes == nil {
		volumes = []corev1.Volume{}
	}
	volumes = append(volumes, corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: name,
				},
			},
		},
	})
	router.Spec.Template.Spec.Volumes = volumes

	//define mount in container
	volumeMounts := router.Spec.Template.Spec.Containers[containerIndex].VolumeMounts
	if volumeMounts == nil {
		volumeMounts = []corev1.VolumeMount{}
	}
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: path,
	})
	router.Spec.Template.Spec.Containers[containerIndex].VolumeMounts = volumeMounts
}

func mountSecretVolume(name string, path string, containerIndex int, router *appsv1.Deployment) {
	//define volume in deployment
	volumes := router.Spec.Template.Spec.Volumes
	if volumes == nil {
		volumes = []corev1.Volume{}
	}
	volumes = append(volumes, corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: name,
			},
		},
	})
	router.Spec.Template.Spec.Volumes = volumes

	//define mount in container
	volumeMounts := router.Spec.Template.Spec.Containers[containerIndex].VolumeMounts
	if volumeMounts == nil {
		volumeMounts = []corev1.VolumeMount{}
	}
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: path,
	})
	router.Spec.Template.Spec.Containers[containerIndex].VolumeMounts = volumeMounts
}

func mountRouterTLSVolume(name string, router *appsv1.Deployment) {
	mountSecretVolume(name, "/etc/qpid-dispatch-certs/" + name + "/", 0, router)
}

func unmountRouterTLSVolume(name string, router *appsv1.Deployment) {
	volumes := []corev1.Volume{}
	for _, v := range router.Spec.Template.Spec.Volumes {
		if v.Name != name {
			volumes = append(volumes, v)
		}
	}
	router.Spec.Template.Spec.Volumes = volumes

	volumeMounts := []corev1.VolumeMount{}
	for _, vm := range router.Spec.Template.Spec.Containers[0].VolumeMounts {
		if vm.Name != name {
			volumeMounts = append(volumeMounts, vm)
		}
	}
	router.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
}

func ensureSaslUsers(user string, password string, owner *metav1.OwnerReference, kube *KubeDetails) {
	name := "skupper-console-users"
	_, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("console users secret already exists")
	} else if errors.IsNotFound(err) {
		secret := corev1.Secret{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Secret",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
			},
			Data: map[string][]byte{
				user: []byte(password),
			},
		}
		if owner != nil {
			secret.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}
		}

		_, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Create(&secret)
		if err != nil {
			log.Fatal("Failed to create console users secret: ", err.Error())
		}
	} else {
		log.Fatal("Failed to check for console users secret: ", err.Error())
	}
}

func ensureSaslConfig(owner *metav1.OwnerReference, kube *KubeDetails) {
	name := "skupper-sasl-config"
	_, err :=kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("sasl config already exists")
	} else if errors.IsNotFound(err) {
		config := `
pwcheck_method: auxprop
auxprop_plugin: sasldb
sasldb_path: /tmp/qdrouterd.sasldb
`
		configMap := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "apps/v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
			},
			Data: map[string]string{
				"qdrouterd.conf": config,
			},
		}
		if owner != nil {
			configMap.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}
		}
		_, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Create(configMap)
		if err != nil {
			log.Fatal("Failed to create sasl config: ", err.Error())
		}
	} else {
		log.Fatal("Failed to check for sasl config: ", err.Error())
	}
}

func addConnector(connector *Connector, router *appsv1.Deployment) {
	config := findEnvVar(router.Spec.Template.Spec.Containers[0].Env, "QDROUTERD_CONF")
	if config == nil {
		log.Fatal("Could not retrieve router config")
	}
	updated := config.Value + connectorConfig(connector)
	setEnvVar(router, "QDROUTERD_CONF", updated)
	mountRouterTLSVolume(connector.Name, router)
}

func messagingServicePorts() []corev1.ServicePort {
	ports := []corev1.ServicePort{}
	ports = append(ports, corev1.ServicePort{
		Name:       "amqps",
		Protocol:   "TCP",
		Port:       5671,
		TargetPort: intstr.FromInt(5671),
	})
	return ports
}

func internalServicePorts() []corev1.ServicePort {
	ports := []corev1.ServicePort{}
	ports = append(ports, corev1.ServicePort{
		Name:       "inter-router",
		Protocol:   "TCP",
		Port:       55671,
		TargetPort: intstr.FromInt(55671),
	})
	ports = append(ports, corev1.ServicePort{
		Name:       "edge",
		Protocol:   "TCP",
		Port:       45671,
		TargetPort: intstr.FromInt(45671),
	})
	return ports
}

func routerPorts(router *Router) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{}
	ports = append(ports, corev1.ContainerPort{
		Name:          "amqps",
		ContainerPort: 5671,
	})
	if router.Console == ConsoleAuthModeOpenshift {
		ports = append(ports, corev1.ContainerPort{
			Name:          "console",
			ContainerPort: 8888,
		})
	} else if router.Console != "" {
		ports = append(ports, corev1.ContainerPort{
			Name:          "console",
			ContainerPort: 8080,
		})
	}
	ports = append(ports, corev1.ContainerPort{
		Name:          "http",
		ContainerPort: 9090,
	})
	if router.Mode == RouterModeInterior {
		ports = append(ports, corev1.ContainerPort{
			Name:          "inter-router",
			ContainerPort: 55671,
		})
		ports = append(ports, corev1.ContainerPort{
			Name:          "edge",
			ContainerPort: 45671,
		})
	}
	return ports
}

func isInterior(router *appsv1.Deployment) bool {
	config := findEnvVar(router.Spec.Template.Spec.Containers[0].Env, "QDROUTERD_CONF")
	//match 'mode: interior' in that config
	if config == nil {
		log.Fatal("Could not retrieve router config")
	}
	match, _ := regexp.MatchString("mode:[ ]+interior", config.Value)
	return match
}

func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for _, v := range env {
		if v.Name == name {
			return &v
		}
	}
	return nil
}

func setEnvVar(router *appsv1.Deployment, name string, value string) {
	original := router.Spec.Template.Spec.Containers[0].Env
	updated := []corev1.EnvVar{}
	for _, v := range original {
		if v.Name == name {
			v.Value = value
			updated = append(updated, corev1.EnvVar{Name: v.Name, Value: value})
		} else {
			updated = append(updated, v)
		}
	}
	router.Spec.Template.Spec.Containers[0].Env = updated
}

func routerEnv(router *Router) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
	if router.Mode == RouterModeInterior {
		envVars = append(envVars, corev1.EnvVar{Name: "APPLICATION_NAME", Value: "skupper-router"})
		envVars = append(envVars, corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.namespace",
			},
		},
		})
		envVars = append(envVars, corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.podIP",
			},
		},
		})
		envVars = append(envVars, corev1.EnvVar{Name: "QDROUTERD_AUTO_MESH_DISCOVERY", Value: "QUERY"})
	}
	envVars = append(envVars, corev1.EnvVar{Name: "QDROUTERD_CONF", Value: routerConfig(router)})
	if router.Console == ConsoleAuthModeInternal {
		envVars = append(envVars, corev1.EnvVar{Name: "QDROUTERD_AUTO_CREATE_SASLDB_SOURCE", Value: "/etc/qpid-dispatch/sasl-users/"})
		envVars = append(envVars, corev1.EnvVar{Name: "QDROUTERD_AUTO_CREATE_SASLDB_PATH", Value: "/tmp/qdrouterd.sasldb"})
	}

	return envVars
}

func routerContainer(router *Router) corev1.Container {
	var image string
	if os.Getenv("QDROUTERD_IMAGE") != "" {
		image = os.Getenv("QDROUTERD_IMAGE")
	} else {
		image = "quay.io/interconnectedcloud/qdrouterd"
	}
	container := corev1.Container{
		Image: image,
		Name:  "router",
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 60,
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Port: intstr.FromInt(9090),
					Path: "/healthz",
				},
			},
		},
		Env:   routerEnv(router),
		Ports: routerPorts(router),
	}
	return container
}

func getLabels(component string) map[string]string{
	//TODO: cleanup handling of labels
	application := "skupper"
	if component == "router" {
		//the automeshing function of the router image expects the application
		//to be used as a unique label for identifying routers to connect to
		application = "skupper-router"
	}
	return map[string]string{
		"application": application,
		"skupper.io/component": component,
	}
}

func RouterDeployment(router *Router, namespace string) *appsv1.Deployment {
	labels := getLabels("router")

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "skupper-router",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &router.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"prometheus.io/port":   "9090",
						"prometheus.io/scrape": "true",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "skupper",
					Containers: []corev1.Container{routerContainer(router)},
				},
			},
		},
	}

	if router.Console == ConsoleAuthModeOpenshift {
		containers := dep.Spec.Template.Spec.Containers
		containers = append(containers, corev1.Container{
			Image: "openshift/oauth-proxy:latest",
			Name:  "oauth-proxy",
			Args: []string{
				"--https-address=:8443",
				"--provider=openshift",
				"--openshift-service-account=skupper",
				"--upstream=http://localhost:8888",
				"--tls-cert=/etc/tls/proxy-certs/tls.crt",
				"--tls-key=/etc/tls/proxy-certs/tls.key",
				"--cookie-secret=SECRET",
			},
			Ports: []corev1.ContainerPort{
				corev1.ContainerPort{
					Name:          "http",
					ContainerPort: 8080,
				},
				corev1.ContainerPort{
					Name:          "https",
					ContainerPort: 8443,
				},
			},

		})
		dep.Spec.Template.Spec.Containers = containers
		mountSecretVolume("skupper-proxy-certs", "/etc/tls/proxy-certs/", 1, dep)
	} else if router.Console == ConsoleAuthModeInternal {
		mountSecretVolume("skupper-console-users", "/etc/qpid-dispatch/sasl-users/", 0, dep)
		mountConfigVolume("skupper-sasl-config", "/etc/sasl2/", 0, dep)
	}

	return dep
}

func randomId(length int) string {
    buffer := make([]byte, length)
    rand.Read(buffer)
    result := base64.StdEncoding.EncodeToString(buffer)
    return result[:length]
}

func ensureProxyController(enableServiceSync bool, router *appsv1.Deployment, kube *KubeDetails) {
	deployments:= kube.Standard.AppsV1().Deployments(kube.Namespace)
	_, err :=  deployments.Get("skupper-proxy-controller", metav1.GetOptions{})
	if err == nil  {
		// Deployment exists, do we need to update it?
		fmt.Println("Proxy controller deployment already exists")
	} else if errors.IsNotFound(err) {

		labels := getLabels("proxy-controller")

		var image string
		if os.Getenv("SKUPPER_PROXY_CONTROLLER_IMAGE") != "" {
			image = os.Getenv("SKUPPER_PROXY_CONTROLLER_IMAGE")
		} else {
			image = "quay.io/skupper/proxy-controller"
		}
		container := corev1.Container{
			Image: image,
			Name:  "proxy-controller",
			Env:   	[]corev1.EnvVar{
				{
					Name: "ICPROXY_SERVICE_ACCOUNT",
					Value: "skupper",
				},
				{
					Name: "OWNER_NAME",
					Value: router.ObjectMeta.Name,
				},
				{
					Name: "OWNER_UID",
					Value: string(router.ObjectMeta.UID),
				},
			},
		}
		var replicas int32
		replicas = 1
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "skupper-proxy-controller",
				OwnerReferences: []metav1.OwnerReference{
					get_owner_reference(router),
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
					},
					Spec: corev1.PodSpec{
						ServiceAccountName: "skupper-proxy-controller",
						Containers: []corev1.Container{container},
					},
				},
			},
		}
		if enableServiceSync {
			origin := randomId(10)
			dep.Spec.Template.Spec.Containers[0].Env = append(dep.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
				Name: "SKUPPER_SERVICE_SYNC_ORIGIN",
				Value: origin,
			})
			mountSecretVolume("skupper", "/etc/messaging/", 0, dep)
		}

		_, err := deployments.Create(dep)
		if err != nil {
			log.Fatal("Failed to create proxy-controller deployment: " + err.Error())
		}
	} else {
		log.Fatal("Failed to check proxy-controller deployment: " + err.Error())
	}
}

func ensureRouterDeployment(router *Router, volumes []string, kube *KubeDetails) *appsv1.Deployment {
	deployments:= kube.Standard.AppsV1().Deployments(kube.Namespace)
	existing, err :=  deployments.Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		// Deployment exists, do we need to update it?
		fmt.Println("Router deployment already exists")
		return existing
	} else if errors.IsNotFound(err) {
		routerDeployment := RouterDeployment(router, kube.Namespace)
		for _, v := range volumes {
			mountRouterTLSVolume(v, routerDeployment)
		}
		created, err := deployments.Create(routerDeployment)
		if err != nil {
			log.Fatal("Failed to create router deployment: " + err.Error())
		} else {
			return created
		}
	} else {
		log.Fatal("Failed to check router deployment: " + err.Error())
	}
	return nil
}

func ensureCA(name string, owner *metav1.OwnerReference, kube *KubeDetails) *corev1.Secret {
	existing, err :=kube.Standard.CoreV1().Secrets(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("CA", name, "already exists")
		return existing
	} else if errors.IsNotFound(err) {
		ca := certs.GenerateCASecret(name, name)
		if owner != nil {
			ca.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}

		}
		_, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Create(&ca)
		if err == nil {
			return &ca
		} else {
			log.Fatal("Failed to create CA", name, ": ", err.Error())
		}
	} else {
		log.Fatal("Failed to check CA", name, ": ", err.Error())
	}
	return nil
}

func ensureService(name string, ports []corev1.ServicePort, owner *metav1.OwnerReference, servingCert string, serviceType string, kube *KubeDetails) (*corev1.Service, error) {
	current, err :=kube.Standard.CoreV1().Services(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("Service", name, "already exists")
		return current, nil
	} else if errors.IsNotFound(err) {
		labels := getLabels("router")
		service := &corev1.Service{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Service",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
			},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    ports,
			},
		}
		if serviceType == string(corev1.ServiceTypeLoadBalancer) {
			service.Spec.Type = corev1.ServiceTypeLoadBalancer
		}
		if servingCert != "" {
			service.ObjectMeta.Annotations = map[string]string{
				"service.alpha.openshift.io/serving-cert-secret-name": servingCert,
			}
		}
		if owner != nil {
			service.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}

		}
		created, err := kube.Standard.CoreV1().Services(kube.Namespace).Create(service)
		if err != nil {
			fmt.Println("Failed to create service", name, ": ", err.Error())
			return nil, err
		} else {
			return created, nil
		}
	} else {
		fmt.Println("Failed while checking service", name, ": ", err.Error())
		return nil, err
	}
}

func ensureRoute(name string, targetService string, targetPort string, termination routev1.TLSTerminationType, owner *metav1.OwnerReference, kube *KubeDetails) string {
	insecurePolicy := routev1.InsecureEdgeTerminationPolicyNone
	if termination != routev1.TLSTerminationPassthrough {
		insecurePolicy = routev1.InsecureEdgeTerminationPolicyRedirect
	}
	_, err := kube.Routes.Routes(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("Route", name, "already exists")
	} else if errors.IsNotFound(err) {
		route := &routev1.Route{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Route",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
			},
			Spec: routev1.RouteSpec{
				Path: "",
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString(targetPort),
				},
				To: routev1.RouteTargetReference{
					Kind: "Service",
					Name: targetService,
				},
				TLS: &routev1.TLSConfig{
					Termination:                   termination,
					InsecureEdgeTerminationPolicy: insecurePolicy,
				},
			},
		}
		if owner != nil {
			route.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}

		}

		created, err := kube.Routes.Routes(kube.Namespace).Create(route)
		if err != nil {
			fmt.Println("Failed to create route", name, ": ", err.Error())
		} else {
			return created.Spec.Host
		}
	} else {
		fmt.Println("Failed while checking route", name, ": ", err.Error())
	}
	return ""
}

func generateSecret(caSecret *corev1.Secret, name string, subject string, hosts string, includeConnectJson bool, owner *metav1.OwnerReference, kube *KubeDetails) {
	secret := certs.GenerateSecret(name, subject, hosts, caSecret)
	if includeConnectJson {
		secret.Data["connect.json"] = []byte(connectJson())
	}
	if owner != nil {
		secret.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			*owner,
		}
	}
	_, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Create(&secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			//TODO: recreate or just use whats there?
			log.Println("Secret", name, "already exists");
		} else {
			log.Fatal("Could not create secret:", err)
		}
	}
}

func ensureServiceAccount(name string, router *appsv1.Deployment, oauth bool, kube *KubeDetails) *corev1.ServiceAccount {
	serviceaccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				get_owner_reference(router),
			},
		},
	}
	if oauth {
		serviceaccount.ObjectMeta.Annotations = map[string]string{
			"serviceaccounts.openshift.io/oauth-redirectreference.primary": "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"skupper-console\"}}",
		}
	}
	actual, err := kube.Standard.CoreV1().ServiceAccounts(kube.Namespace).Create(serviceaccount)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Service account", name, "already exists");
		} else {
			log.Fatal("Could not create service account", name, ":", err)
		}

	}
	return actual
}

func ensureViewRole(router *appsv1.Deployment, kube *KubeDetails) *rbacv1.Role {
	name := "skupper-view"
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				get_owner_reference(router),
			},
		},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"get", "list", "watch"},
			APIGroups: []string{""},
			Resources: []string{"pods"},
		}},
	}
	actual, err := kube.Standard.RbacV1().Roles(kube.Namespace).Create(role)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role", name, "already exists");
		} else {
			log.Fatal("Could not create role", name, ":", err)
		}

	}
	return actual
}

func ensureEditRole(router *appsv1.Deployment, kube *KubeDetails) *rbacv1.Role {
	name := "skupper-edit"
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				get_owner_reference(router),
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				APIGroups: []string{""},
				Resources: []string{"services"},
			},
			{
				Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
			},
		},
	}
	actual, err := kube.Standard.RbacV1().Roles(kube.Namespace).Create(role)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role", name, "already exists");
		} else {
			log.Fatal("Could not create role", name, ":", err)
		}

	}
	return actual
}

func ensureRoleBinding(serviceaccount string, role string, router *appsv1.Deployment, kube *KubeDetails) {
	name := serviceaccount + "-" + role
	rolebinding := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			OwnerReferences: []metav1.OwnerReference{
				get_owner_reference(router),
			},
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount",
			Name: serviceaccount,
		}},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role",
			Name: role,
		},
	}
	_, err := kube.Standard.RbacV1().RoleBindings(kube.Namespace).Create(rolebinding)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role binding", name, "already exists");
		} else {
			log.Fatal("Could not create role binding", name, ":", err)
		}

	}
}

func get_owner_reference(dep *appsv1.Deployment) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       dep.ObjectMeta.Name,
		UID:        dep.ObjectMeta.UID,
	}

}

func initCommon(router *Router, volumes []string, kube *KubeDetails) *appsv1.Deployment {
	if router.Name == "" {
		router.Name = kube.Namespace
	}
	dep := ensureRouterDeployment(router, volumes, kube)
	owner := get_owner_reference(dep)

	ca := ensureCA("skupper-ca", &owner, kube)
	generateSecret(ca, "skupper-amqps", "skupper-messaging", "skupper-messaging,skupper-messaging." + kube.Namespace + ".svc.cluster.local", false, &owner, kube)
	generateSecret(ca, "skupper", "skupper-messaging", "", true, &owner, kube)
	ensureServiceAccount("skupper", dep, router.Console == ConsoleAuthModeOpenshift, kube)
	ensureViewRole(dep, kube)
	ensureRoleBinding("skupper", "skupper-view", dep, kube)


	ensureService("skupper-messaging", messagingServicePorts(), &owner, "", "", kube)
	if router.Console != "" {
		servingCerts := ""
		termination := routev1.TLSTerminationEdge
		port := corev1.ServicePort{
			Name:       "console",
			Protocol:   "TCP",
			Port:       8080,
			TargetPort: intstr.FromInt(8080),
		}

		if router.Console == ConsoleAuthModeOpenshift {
			servingCerts = "skupper-proxy-certs"
			termination = routev1.TLSTerminationReencrypt
			port = corev1.ServicePort{
				Name:       "console",
				Protocol:   "TCP",
				Port:       443,
				TargetPort: intstr.FromInt(8443),
			}
		} else if router.Console == ConsoleAuthModeInternal {
			ensureSaslConfig(&owner, kube)
			ensureSaslUsers(router.ConsoleUser, router.ConsolePassword, &owner, kube)
		}
		ensureService("skupper-console", []corev1.ServicePort{
			port,
		}, &owner, servingCerts, "", kube)
		if kube.Routes != nil {
			ensureRoute("skupper-console", "skupper-console", "console", termination, &owner, kube)
		} //else ??TODO?? create ingress
	}

	return dep
}

func initProxyController(enableServiceSync bool, router *appsv1.Deployment, kube *KubeDetails) {
	ensureServiceAccount("skupper-proxy-controller", router, false, kube)
	ensureEditRole(router, kube)
	ensureRoleBinding("skupper-proxy-controller", "skupper-edit", router, kube)
	ensureProxyController(enableServiceSync, router, kube)
}

func initEdge(router *Router, kube *KubeDetails) *appsv1.Deployment {
	return initCommon(router, []string{"skupper-amqps"}, kube)
}

func initInterior(router *Router, kube *KubeDetails, clusterLocal bool) *appsv1.Deployment {
	dep := initCommon(router, []string{"skupper-amqps", "skupper-internal"}, kube)
	owner := get_owner_reference(dep)
	internalCa := ensureCA("skupper-internal-ca", &owner, kube)
	if clusterLocal {
		ensureService("skupper-internal", internalServicePorts(), &owner, "", "", kube)
		generateSecret(internalCa, "skupper-internal", "skupper-internal", "skupper-internal." + kube.Namespace, false, &owner, kube)
	} else if kube.Routes != nil {
		ensureService("skupper-internal", internalServicePorts(), &owner, "", "", kube)
		//TODO: handle loadbalancer service where routes are not available
		hosts := ensureRoute("skupper-inter-router", "skupper-internal", "inter-router", routev1.TLSTerminationPassthrough, &owner, kube)
		hosts += "," + ensureRoute("skupper-edge", "skupper-internal", "edge", routev1.TLSTerminationPassthrough, &owner, kube)
		generateSecret(internalCa, "skupper-internal", router.Name, hosts, false, &owner, kube)
	} else {
		service, err := ensureService("skupper-internal", internalServicePorts(), &owner, "", "LoadBalancer", kube)
		if err == nil {
			host := getLoadBalancerHostOrIp(service)
			for i := 0; host == "" && i < 120; i++ {
				if i == 0 {
					fmt.Println("Waiting for LoadBalancer IP or hostname...")
				}
				time.Sleep(time.Second)
				service, err = kube.Standard.CoreV1().Services(kube.Namespace).Get("skupper-internal", metav1.GetOptions{})
				host = getLoadBalancerHostOrIp(service)
			}
			if host == "" {
				log.Fatal("Could not get LoadBalancer IP or Hostname for service skupper-internal. Retry after resolving or run init with --cluster-local or --edge.")
			} else {
				if len(host) < 64 {
					generateSecret(internalCa, "skupper-internal", host, host, false, &owner, kube)
				} else {
					generateSecret(internalCa, "skupper-internal", router.Name, host, false, &owner, kube)
				}
			}
		}
	}
	return dep
}


func deleteSecret(name string, kube *KubeDetails) {
	secrets:= kube.Standard.CoreV1().Secrets(kube.Namespace)
	err :=  secrets.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Secret", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Secret", name, "does not exist")
	} else {
		fmt.Println("Failed to delete secret", name, ": ", err.Error())
	}
}

func deleteSkupper(kube *KubeDetails) {
	err := kube.Standard.AppsV1().Deployments(kube.Namespace).Delete("skupper-router", &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Skupper is now removed from '" + kube.Namespace + "'.")
	} else if errors.IsNotFound(err) {
		fmt.Println("Skupper not installed in '" + kube.Namespace + "'.")
	} else {
		fmt.Println("Error while trying to delete:", err.Error())
	}
}

func list_connectors(mode RouterMode, kube *KubeDetails) []Connector {
	var connectors []Connector
	secrets, err := kube.Standard.CoreV1().Secrets(kube.Namespace).List(metav1.ListOptions{LabelSelector:"skupper.io/type=connection-token",})
	if err == nil {
		var role ConnectorRole
		var hostKey string
		var portKey string
		if mode == RouterModeEdge {
			role = ConnectorRoleEdge
			hostKey = "edge-host"
			portKey = "edge-port"
		} else {
			role = ConnectorRoleInterRouter
			hostKey = "inter-router-host"
			portKey = "inter-router-port"
		}
		for _, s := range secrets.Items {
			connectors = append(connectors, Connector{
				Name: s.ObjectMeta.Name,
				Host: s.ObjectMeta.Annotations[hostKey],
				Port: s.ObjectMeta.Annotations[portKey],
				Role: role,
			})
		}
	} else {
		log.Fatal("Could not retrieve connection-token secrets:", err)
	}
	return connectors
}

func generate_connector_name(kube *KubeDetails) string {
	secrets, err := kube.Standard.CoreV1().Secrets(kube.Namespace).List(metav1.ListOptions{LabelSelector:"skupper.io/type=connection-token",})
	max := 1
	if err == nil {
		connector_name_pattern := regexp.MustCompile("conn([0-9])+")
		for _, s := range secrets.Items {
			count := connector_name_pattern.FindStringSubmatch(s.ObjectMeta.Name)
			if len(count) > 1 {
				v, _ := strconv.Atoi(count[1])
				if v >= max {
					max = v + 1
				}
			}

		}
	} else {
		log.Fatal("Could not retrieve connection-token secrets:", err)
	}
	return "conn" + strconv.Itoa(max)
}

func get_router_mode(router *appsv1.Deployment) RouterMode {
	if isInterior(router) {
		return RouterModeInterior
	} else {
		return RouterModeEdge
	}
}

func status(kube *KubeDetails, listConnectors bool) {
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		mode := get_router_mode(current)
		var modedesc string
		if mode == RouterModeEdge {
			modedesc = " in edge mode"
		}
		if current.Status.ReadyReplicas == 0 {
			fmt.Printf("Skupper enabled for namespace %q%s. Status pending...", kube.Namespace, modedesc)
		} else {
			connected, err := router.GetConnectedSites(kube.Namespace, kube.Standard, kube.RestConfig)
			for i :=0; i < 5 && err != nil; i++ {
				time.Sleep(500*time.Millisecond)
				connected, err = router.GetConnectedSites(kube.Namespace, kube.Standard, kube.RestConfig)
			}
			if err != nil {
				log.Fatalf("Skupper enabled for namespace %s. Unable to determine connectivity:%s\n", kube.Namespace, err)
			} else {
				if connected.Total == 0 {
					fmt.Printf("Skupper enabled for namespace %q%s. It is not connected to any other sites.", kube.Namespace, modedesc)
				} else if connected.Total == 1 {
					fmt.Printf("Skupper enabled for namespace %q%s. It is connected to 1 other site.", kube.Namespace, modedesc)
				} else if connected.Total == connected.Direct {
					fmt.Printf("Skupper enabled for namespace %q%s. It is connected to %d other sites.", kube.Namespace, modedesc, connected.Total)
				} else {
					fmt.Printf("Skupper enabled for namespace %q%s. It is connected to %d other sites (%d indirectly).", kube.Namespace, modedesc, connected.Total, connected.Indirect)
				}
			}
		}
		fmt.Println()
		if listConnectors {
			fmt.Println()
			connectors := list_connectors(mode, kube)
			if len(connectors) == 0 {
				fmt.Println("There are no connectors defined.")
			} else {
				fmt.Println("Connectors:")
				for _, c := range connectors {
					fmt.Printf("    %s:%s (name=%s)", c.Host, c.Port, c.Name)
					fmt.Println()
				}
			}
		}
	} else if errors.IsNotFound(err) {
		fmt.Println("skupper not enabled for", kube.Namespace)
	} else {
		log.Fatal(err)
	}
}

func remove_connector(name string, list []Connector) (bool, []Connector) {
	updated := []Connector{}
	found := false
	for _, c := range list {
		if c.Name != name {
			updated = append(updated, c)
		} else {
			found = true
		}
	}
	return found, updated
}

func disconnect(name string, kube *KubeDetails) {
	router, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		mode := get_router_mode(router)
		found, connectors := remove_connector(name, list_connectors(mode, kube))
		if (found) {
			config := findEnvVar(router.Spec.Template.Spec.Containers[0].Env, "QDROUTERD_CONF")
			if config == nil {
				log.Fatal("Could not retrieve router config")
			} else {
				pattern := "## Connectors: ##"
				updated := strings.Split(config.Value, pattern)[0] + pattern
				for _, c := range connectors {
					updated += connectorConfig(&c)
				}
				setEnvVar(router, "QDROUTERD_CONF", updated)
				unmountRouterTLSVolume(name, router)
				deleteSecret(name, kube)
				_, err = kube.Standard.AppsV1().Deployments(kube.Namespace).Update(router)
				if err != nil {
					fmt.Println("Failed to remove connection:", err.Error())
				}
			}
		} else {
			fmt.Println("connection", name, "not found")
		}
	} else if errors.IsNotFound(err) {
		fmt.Println("skupper not enabled in", kube.Namespace)
	} else {
		log.Fatal(err)
	}
}

func connect(secretFile string, connectorName string, kube *KubeDetails) {
	yaml, err := ioutil.ReadFile(secretFile)
        if err != nil {
		fmt.Printf("Could not read connection token: %s", err)
		fmt.Println()
		return
        }
	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme,
                scheme.Scheme)
        var secret corev1.Secret
        _, _, err = s.Decode(yaml, nil, &secret)
        if err != nil {
                fmt.Printf("Could not parse connection token: %s", err)
		fmt.Println()
		return
        }
	//determine if local deployment is edge or interior
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		mode := get_router_mode(current)
		if connectorName == "" {
			connectorName = generate_connector_name(kube)
		}
		secret.ObjectMeta.Name = connectorName
		secret.ObjectMeta.Labels = map[string]string{
			"skupper.io/type": "connection-token",
		}
		secret.ObjectMeta.SetOwnerReferences([]metav1.OwnerReference{
			get_owner_reference(current),
		});
		_, err = kube.Standard.CoreV1().Secrets(kube.Namespace).Create(&secret)
		if err == nil {
			//read annotations to get the host and port to connect to
			connector := Connector{
				Name: connectorName,
			}
			if mode == RouterModeInterior {
				connector.Host = secret.ObjectMeta.Annotations["inter-router-host"]
				connector.Port = secret.ObjectMeta.Annotations["inter-router-port"]
				connector.Role = ConnectorRoleInterRouter
			} else {
				connector.Host = secret.ObjectMeta.Annotations["edge-host"]
				connector.Port = secret.ObjectMeta.Annotations["edge-port"]
				connector.Role = ConnectorRoleEdge
			}
			addConnector(&connector, current)
			_, err = kube.Standard.AppsV1().Deployments(kube.Namespace).Update(current)
			if err != nil {
				fmt.Println("Failed to update router deployment: ", err.Error())
			} else {
				fmt.Printf("Skupper configured to connect to %s:%s (name=%s)", connector.Host, connector.Port, connector.Name)
				fmt.Println()
			}
		} else if errors.IsAlreadyExists(err) {
			fmt.Println("A connector secret of that name already exist, please choose a different name")
		} else {
			fmt.Println("Failed to create connector secret: ", err.Error())
		}
	} else {
		fmt.Println("Failed to retrieve router deployment: ", err.Error())
	}
}

func annotateConnectionToken(secret *corev1.Secret, role string, host string, port string) {
	if secret.ObjectMeta.Annotations == nil {
		secret.ObjectMeta.Annotations = map[string]string{}
	}
	secret.ObjectMeta.Annotations[role + "-host"] = host
	secret.ObjectMeta.Annotations[role + "-port"] = port
}

func getLoadBalancerHostOrIp(service *corev1.Service) string {
	for _, i := range service.Status.LoadBalancer.Ingress {
		if i.IP != "" {
			return i.IP
		} else if i.Hostname != "" {
			return i.Hostname
		}
	}
	return ""
}

func getLoadBalancerNodePort(service *corev1.Service, name string) string {
	for _, p := range service.Spec.Ports {
		if p.Name == name {
			if p.NodePort > 0 {
				return strconv.Itoa(int(p.NodePort))
			} else {
				return ""
			}
		}
	}
	return ""
}

type HostPort struct {
	Host string
	Port string
}

type RouterHostPorts struct {
	Edge        HostPort
	InterRouter HostPort
	Hosts       string
	LocalOnly   bool
}

func configureHostPortsFromRoutes(result *RouterHostPorts, kube *KubeDetails) (bool, error) {
	if kube.Routes == nil {
		return false, nil
	} else {
		interRouterRoute, err1 := kube.Routes.Routes(kube.Namespace).Get("skupper-inter-router", metav1.GetOptions{})
		edgeRoute, err2 := kube.Routes.Routes(kube.Namespace).Get("skupper-edge", metav1.GetOptions{})
		if err1 != nil && err2 != nil && errors.IsNotFound(err1) && errors.IsNotFound(err2) {
			return false, nil
		} else if err1 != nil {
			return false, err1
		} else if err2 != nil {
			return false, err2
		} else {
			result.Edge.Host = edgeRoute.Spec.Host
			result.Edge.Port = "443"
			result.InterRouter.Host = interRouterRoute.Spec.Host
			result.InterRouter.Port = "443"
			result.Hosts = edgeRoute.Spec.Host + "," + interRouterRoute.Spec.Host
			return true, nil
		}
	}
}

func configureHostPorts(result *RouterHostPorts, kube *KubeDetails) bool {
	ok, err := configureHostPortsFromRoutes(result, kube)
	if err != nil {
		log.Fatal("Could not get routes", err.Error())
		return false
	} else if ok {
		return ok
	} else {
		service, err := kube.Standard.CoreV1().Services(kube.Namespace).Get("skupper-internal", metav1.GetOptions{})
		if err != nil {
			log.Fatal("Could not get service", err.Error())
			return false
		} else {
			if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
				host := getLoadBalancerHostOrIp(service)
				if host != "" {
					result.Hosts = host
					result.InterRouter.Host = host
					result.InterRouter.Port = "55671"
					result.Edge.Host = host
					result.Edge.Port = "45671"
					return true
				} else {
					fmt.Printf("LoadBalancer Host/IP not yet allocated for service %s, ", service.ObjectMeta.Name)
				}
			}
			result.LocalOnly = true
			host := fmt.Sprintf("skupper-internal.%s", kube.Namespace)
			result.Hosts = host
			result.InterRouter.Host = host
			result.InterRouter.Port = "55671"
			result.Edge.Host = host
			result.Edge.Port = "45671"
			return true
		}
	}
}

func generateConnectSecret(subject string, secretFile string, kube *KubeDetails) {
	//verify that local deployment is interior
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		if isInterior(current) {
			caSecret, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Get("skupper-internal-ca", metav1.GetOptions{})
			if err == nil {
				//get the host and port for inter-router and edge
				var hostPorts RouterHostPorts
				if configureHostPorts(&hostPorts, kube) {
					secret := certs.GenerateSecret(subject, subject, hostPorts.Hosts, caSecret)
					//add annotations for host and port for both edge and inter-router connections
					annotateConnectionToken(&secret, "inter-router", hostPorts.InterRouter.Host, hostPorts.InterRouter.Port)
					annotateConnectionToken(&secret, "edge", hostPorts.Edge.Host, hostPorts.Edge.Port)
					//generate yaml and save it to the specified path
					s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
					out, err := os.Create(secretFile)
					if err != nil {
						log.Fatal("Could not write to file " + secretFile + ": " + err.Error())
					}
					err = s.Encode(&secret, out)
					if err != nil {
						log.Fatal("Could not write out generated secret: " + err.Error())
					} else {
						var extra string
						if hostPorts.LocalOnly {
							extra = "(Note: token will only be valid for local cluster)"
						}
						fmt.Printf("Connection token written to %s %s", secretFile, extra)
						fmt.Println()

					}
				}
			} else if errors.IsNotFound(err) {
				fmt.Println("Internal CA does not exist: " + err.Error())
			} else {
				fmt.Println("Error retrieving internal CA: " + err.Error())
			}
		} else {
			fmt.Println("Edge configuration cannot accept connections")
		}
	} else if errors.IsNotFound(err) {
		fmt.Println("Router deployment does not exist (need init?): " + err.Error())
	} else {
		fmt.Println("Error retrieving router deployment: " + err.Error())
	}
}

func requiredArg(name string) func(*cobra.Command,[]string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("%s must be specified", name)
		}
		if len(args) > 1 {
			return fmt.Errorf("illegal argument: %s", args[1])
		}
		return nil
	}
}

type KubeDetails struct {
	Namespace string
	Standard *kubernetes.Clientset
	Routes *routev1client.RouteV1Client
	RestConfig *restclient.Config
}

func initKubeConfig(namespace string, context string) *KubeDetails {
	details := KubeDetails{}

        kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
                clientcmd.NewDefaultClientConfigLoadingRules(),
                &clientcmd.ConfigOverrides{
			CurrentContext:context,
		},
        )
        restconfig, err := kubeconfig.ClientConfig()
        if err != nil {
                log.Fatal(err)
        }

	restconfig.ContentConfig.GroupVersion = &schema.GroupVersion{Version:"v1"}
	restconfig.APIPath = "/api"
	restconfig.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	details.RestConfig = restconfig
        details.Standard, err = kubernetes.NewForConfig(restconfig)
        if err != nil {
                log.Fatal(err)
        }
	dc, err := discovery.NewDiscoveryClientForConfig(restconfig)
	resources, err := dc.ServerResourcesForGroupVersion("route.openshift.io/v1")
	if err == nil && len(resources.APIResources) > 0 {
		details.Routes, err = routev1client.NewForConfig(restconfig)
		if err != nil {
			log.Fatal(err.Error())
		}
	}

	if namespace == "" {
		details.Namespace, _, err = kubeconfig.Namespace()
		if err != nil {
			log.Fatal(err)
		}
	} else {
		details.Namespace = namespace
	}

	return &details
}

func main() {
	routev1.AddToScheme(scheme.Scheme)
	routev1.AddToSchemeInCoreGroup(scheme.Scheme)

	var context string
	var namespace string

	var skupperName string
	var isEdge bool
	var enableProxyController bool
	var enableServiceSync bool
	var enableRouterConsole bool
	var routerConsoleAuthMode string
	var routerConsoleUser string
	var routerConsolePassword string
	var clusterLocal bool
	var cmdInit = &cobra.Command{
		Use:   "init",
		Short: "Initialise skupper installation",
		Long: `init will setup a router and other supporting objects to provide a functional skupper installation that can then be connected to other skupper installations`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			router := Router{
				Name: skupperName,
				Mode: RouterModeInterior,
				Replicas: 1,
			}
			if enableRouterConsole {
				if routerConsoleAuthMode == string(ConsoleAuthModeInternal) || routerConsoleAuthMode == "" {
					router.Console = ConsoleAuthModeInternal
					router.ConsoleUser = routerConsoleUser
					router.ConsolePassword = routerConsolePassword
					if router.ConsoleUser == "" {
						router.ConsoleUser = "admin"
					}
					if router.ConsolePassword == "" {
						router.ConsolePassword = randomId(10)
					}
				} else {
					if routerConsoleUser != "" {
						log.Fatal("--router-console-user only valid when --router-console-auth=internal")
					}
					if routerConsolePassword != "" {
						log.Fatal("--router-console-password only valid when --router-console-auth=internal")
					}
					if routerConsoleAuthMode == string(ConsoleAuthModeOpenshift) {
						router.Console = ConsoleAuthModeOpenshift
					} else if routerConsoleAuthMode == string(ConsoleAuthModeUnsecured) {
						router.Console = ConsoleAuthModeUnsecured
					} else {
						log.Fatal("Unrecognised router console authentication mode: ", routerConsoleAuthMode)
					}
				}
			}

			kube := initKubeConfig(namespace, context)
			var dep *appsv1.Deployment
			if !isEdge {
				dep = initInterior(&router, kube, clusterLocal)
			} else {
				router.Mode = RouterModeEdge
				dep = initEdge(&router, kube)
			}
			if enableProxyController {
				initProxyController(enableServiceSync, dep, kube)
			}
			fmt.Println("Skupper is now installed in namespace '" + kube.Namespace + "'.  Use 'skupper status' to get more information.")
		},
	}
	cmdInit.Flags().StringVarP(&skupperName, "id", "", "", "Provide a specific identity for the skupper installation")
	cmdInit.Flags().BoolVarP(&isEdge, "edge", "", false, "Configure as an edge")
	cmdInit.Flags().BoolVarP(&enableProxyController, "enable-proxy-controller", "", true, "Setup the proxy controller as well as the router")
	cmdInit.Flags().BoolVarP(&enableServiceSync, "enable-service-sync", "", true, "Configure proxy controller to particiapte in service sync (not relevant if --enable-proxy-controller is false)")
	cmdInit.Flags().BoolVarP(&enableRouterConsole, "enable-router-console", "", false, "Enable router console")
	cmdInit.Flags().StringVarP(&routerConsoleAuthMode, "router-console-auth", "", "", "Authentication mode for router console. One of: 'openshift', 'internal', 'unsecured'")
	cmdInit.Flags().StringVarP(&routerConsoleUser, "router-console-user", "", "", "Router console user. Valid only when --router-console-auth=internal")
	cmdInit.Flags().StringVarP(&routerConsolePassword, "router-console-password", "", "", "Router console user. Valid only when --router-console-auth=internal")
	cmdInit.Flags().BoolVarP(&clusterLocal, "cluster-local", "", false, "Set up skupper to only accept connections from within the local cluster.")

	var cmdDelete = &cobra.Command{
		Use:   "delete",
		Short: "Delete skupper installation",
		Long: `delete will delete any skupper related objects from the namespace`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			deleteSkupper(initKubeConfig(namespace, context))
		},
	}


	var clientIdentity string
	var cmdConnectionToken = &cobra.Command{
		Use:   "connection-token <output-file>",
		Short: "Create a connection token file with which another skupper installation can connect to this one",
		Args: requiredArg("output-file"),
		Run: func(cmd *cobra.Command, args []string) {
			generateConnectSecret(clientIdentity, args[0], initKubeConfig(namespace, context))
		},
	}
	cmdConnectionToken.Flags().StringVarP(&clientIdentity, "client-identity", "i", "skupper", "Provide a specific identity as which connecting skupper installation will be authenticated")

	var connectionName string
	var cmdConnect = &cobra.Command{
		Use:   "connect <connection-token-file>",
		Short: "Connect this skupper installation to that which issued the specified connectionToken",
		Args: requiredArg("connection-token"),
		Run: func(cmd *cobra.Command, args []string) {
			connect(args[0], connectionName, initKubeConfig(namespace, context))
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "connection-name", "", "", "Provide a specific name for the connection (used when removing it with disconnect)")

	var cmdDisconnect = &cobra.Command{
		Use:   "disconnect <name>",
		Short: "Remove specified connection",
		Args: requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			disconnect(args[0], initKubeConfig(namespace, context))
		},
	}

	var listConnectors bool
	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "Report status of skupper installation",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			status(initKubeConfig(namespace, context), listConnectors)
		},
	}
	cmdStatus.Flags().BoolVarP(&listConnectors, "list-connectors", "", false, "List configured outgoing connections")

	var cmdVersion = &cobra.Command{
		Use:   "version",
		Short: "Report version of skupper cli and services",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			kubeConfig := initKubeConfig(namespace, context)
			routerVersion := kube.GetComponentVersion(kubeConfig.Namespace, kubeConfig.Standard, "router")
			proxyControllerVersion := kube.GetComponentVersion(kubeConfig.Namespace, kubeConfig.Standard, "proxy-controller")
			fmt.Printf("client version           %s\n", version)
			fmt.Printf("router version           %s\n", routerVersion)
			fmt.Printf("proxy-controller version %s\n", proxyControllerVersion)
		},
	}

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.Version = version
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdConnectionToken, cmdConnect, cmdDisconnect, cmdStatus, cmdVersion)
	rootCmd.PersistentFlags().StringVarP(&context, "context", "c", "", "kubeconfig context to use")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "kubernetes namespace to use")
	rootCmd.Execute()
}
