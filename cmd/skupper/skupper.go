package main

import (
	"bytes"
	"crypto/rand"
	jsonencoding "encoding/json"
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
	ConsoleEnabled bool
	ConsoleAuthMode ConsoleAuthMode
	ConsoleUser string
	ConsolePassword string
}

func routerConfig(router *Router) string {
	config := `
router {
    mode: {{.Mode}}
    id: {{.Name}}-${HOSTNAME}
    metadata: ${SKUPPER_SITE_ID}
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

{{- if .ConsoleEnabled }}
{{- if eq .ConsoleAuthMode "openshift"}}
# console secured by oauth proxy sidecar
listener {
    host: localhost
    port: 8888
    role: normal
    http: true
}
{{- else if eq .ConsoleAuthMode "internal"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
    authenticatePeer: true
}
{{- else if eq .ConsoleAuthMode "unsecured"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
}
{{- end }}
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
	Cost int
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
    cost: {{.Cost}}
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

func mountSecretVolume(name string, path string, containerIndex int, deployment *appsv1.Deployment) {
	//define volume in deployment
	volumes := deployment.Spec.Template.Spec.Volumes
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
	deployment.Spec.Template.Spec.Volumes = volumes

	//define mount in container
	volumeMounts := deployment.Spec.Template.Spec.Containers[containerIndex].VolumeMounts
	if volumeMounts == nil {
		volumeMounts = []corev1.VolumeMount{}
	}
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: path,
	})
	deployment.Spec.Template.Spec.Containers[containerIndex].VolumeMounts = volumeMounts
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
		Name:         "amqps",
		ContainerPort: 5671,
	})
	if router.ConsoleEnabled {
		if router.ConsoleAuthMode == ConsoleAuthModeOpenshift {
			ports = append(ports, corev1.ContainerPort{
				Name:          "console",
				ContainerPort: 8888,
			})
		} else if router.ConsoleAuthMode != "" {
			ports = append(ports, corev1.ContainerPort{
				Name:          "console",
				ContainerPort: 8080,
			})
		}
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
	envVars = append(envVars, corev1.EnvVar{
		Name: "SKUPPER_SITE_ID",
		ValueFrom: &corev1.EnvVarSource {
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector {
				LocalObjectReference: corev1.LocalObjectReference {
					"skupper-site",
				},
				Key: "id",
			},
		},
	})

	if router.ConsoleEnabled && router.ConsoleAuthMode == ConsoleAuthModeInternal {
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

func OauthProxyContainer(serviceAccount string) *corev1.Container {
	return &corev1.Container{
		Image: "openshift/oauth-proxy:latest",
		Name:  "oauth-proxy",
		Args: []string{
			"--https-address=:8443",
			"--provider=openshift",
			"--openshift-service-account=" + serviceAccount,
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
	}
}

func RouterDeployment(router *Router, owner *metav1.OwnerReference, namespace string) *appsv1.Deployment {
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
	if owner != nil {
		dep.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			*owner,
		}
	}

	if router.ConsoleEnabled && router.ConsoleAuthMode == ConsoleAuthModeOpenshift {
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
	} else if router.ConsoleEnabled && router.ConsoleAuthMode == ConsoleAuthModeInternal {
		mountSecretVolume("skupper-console-users", "/etc/qpid-dispatch/sasl-users/", 0, dep)
		mountConfigVolume("skupper-sasl-config", "/etc/sasl2/", 0, dep)
	}

	return dep
}

const alphanumerics = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomId(length int) string {
	buffer := make([]byte, length)
	rand.Read(buffer)
	max := len(alphanumerics)
	for i := range buffer {
		buffer[i] = alphanumerics[int(buffer[i]) % max]
	}
	return string(buffer)
}

func ensureProxyController(enableServiceSync bool, router *appsv1.Deployment, authConfig *Router, kube *KubeDetails) {
	deployments:= kube.Standard.AppsV1().Deployments(kube.Namespace)
	_, err :=  deployments.Get("skupper-proxy-controller", metav1.GetOptions{})
	if err == nil  {
		// Deployment exists, do we need to update it?
		fmt.Println("Proxy controller deployment already exists")
	} else if errors.IsNotFound(err) {

		labels := getLabels("proxy-controller")

		var image string
		if os.Getenv("SKUPPER_CONTROLLER_IMAGE") != "" {
			image = os.Getenv("SKUPPER_CONTROLLER_IMAGE")
		} else {
			image = "quay.io/skupper/controller:0.2.0"
		}
		var proxyImage string
		if os.Getenv("SKUPPER_PROXY_IMAGE") != "" {
			proxyImage = os.Getenv("SKUPPER_PROXY_IMAGE")
		} else {
			proxyImage = "quay.io/skupper/proxy:0.2.0"
		}
		container := corev1.Container{
			Image: image,
			Name:  "proxy-controller",
			Env:   	[]corev1.EnvVar{
				{
					Name: "SKUPPER_PROXY_IMAGE",
					Value: proxyImage,
				},
				{
					Name: "SKUPPER_SERVICE_ACCOUNT",
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

		if authConfig.ConsoleAuthMode == ConsoleAuthModeOpenshift {
			dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, *OauthProxyContainer("skupper-proxy-controller"))
			env := &dep.Spec.Template.Spec.Containers[0].Env
			*env = append(*env, corev1.EnvVar {
				Name: "METRICS_PORT",
				Value: "8888",
			})
			*env = append(*env, corev1.EnvVar {
				Name: "METRICS_HOST",
				Value: "localhost",
			})
			mountSecretVolume("skupper-controller-certs", "/etc/tls/proxy-certs/", 1, dep)
		} else if authConfig.ConsoleAuthMode == ConsoleAuthModeInternal {
			env := &dep.Spec.Template.Spec.Containers[0].Env
			mountSecretVolume("skupper-console-users", "/etc/console-users", 0, dep)
			*env = append(*env, corev1.EnvVar {
				Name: "METRICS_USERS",
				Value: "/etc/console-users",
			})
		}

		if enableServiceSync {
			dep.Spec.Template.Spec.Containers[0].Env = append(dep.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
				Name: "SKUPPER_SERVICE_SYNC_ORIGIN",
				ValueFrom: &corev1.EnvVarSource {
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector {
						LocalObjectReference: corev1.LocalObjectReference {
							"skupper-site",
						},
						Key: "id",
					},
				},
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

func ensureRouterDeployment(router *Router, volumes []string, owner *metav1.OwnerReference, kube *KubeDetails) *appsv1.Deployment {
	deployments:= kube.Standard.AppsV1().Deployments(kube.Namespace)
	existing, err :=  deployments.Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		// Deployment exists, do we need to update it?
		fmt.Println("Router deployment already exists")
		return existing
	} else if errors.IsNotFound(err) {
		routerDeployment := RouterDeployment(router, owner, kube.Namespace)
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

func ensureServiceForRouter(name string, ports []corev1.ServicePort, owner *metav1.OwnerReference, servingCert string, serviceType string, kube *KubeDetails) (*corev1.Service, error) {
	return ensureServiceForComponent(name, "router", ports, owner, servingCert, serviceType, kube)
}

func ensureServiceForController(name string, ports []corev1.ServicePort, owner *metav1.OwnerReference, servingCert string, serviceType string, kube *KubeDetails) (*corev1.Service, error) {
	return ensureServiceForComponent(name, "proxy-controller", ports, owner, servingCert, serviceType, kube)
}

func ensureServiceForComponent(name string, component string, ports []corev1.ServicePort, owner *metav1.OwnerReference, servingCert string, serviceType string, kube *KubeDetails) (*corev1.Service, error) {
	current, err :=kube.Standard.CoreV1().Services(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("Service", name, "already exists")
		return current, nil
	} else if errors.IsNotFound(err) {
		labels := getLabels(component)
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

func ensureServiceAccount(name string, router *appsv1.Deployment, oauth string, kube *KubeDetails) *corev1.ServiceAccount {
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
	if oauth != "" {
		serviceaccount.ObjectMeta.Annotations = map[string]string{
			"serviceaccounts.openshift.io/oauth-redirectreference.primary": "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"" + oauth + "\"}}",
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
				Resources: []string{"services", "configmaps", "pods"},
			},
			{
				Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "statefulsets"},
			},
		},
	}
	if kube.Routes != nil {
		role.Rules = append(role.Rules,
			rbacv1.PolicyRule {
				Verbs:     []string{"get"},
				APIGroups: []string{"route.openshift.io"},
				Resources: []string{"routes"},
			},
		)
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

func get_owner_reference_cm(config *corev1.ConfigMap) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "core/v1",
		Kind:       "ConfigMap",
		Name:       config.ObjectMeta.Name,
		UID:        config.ObjectMeta.UID,
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

func initCommon(router *Router, volumes []string, clusterLocal bool, kube *KubeDetails) *appsv1.Deployment {
	if router.Name == "" {
		router.Name = kube.Namespace
	}
	siteconfig, err := ensureSkupperConfig(router, kube)
	if err != nil {
		log.Fatal("Could not initialise skupper site")
		return nil
	}
	owner := get_owner_reference_cm(siteconfig)
	dep := ensureRouterDeployment(router, volumes, &owner, kube)

	ca := ensureCA("skupper-ca", &owner, kube)
	generateSecret(ca, "skupper-amqps", "skupper-messaging", "skupper-messaging,skupper-messaging." + kube.Namespace + ".svc.cluster.local", false, &owner, kube)
	generateSecret(ca, "skupper", "skupper-messaging", "", true, &owner, kube)
	oauthRouteName := ""
	if router.ConsoleAuthMode == ConsoleAuthModeOpenshift {
		oauthRouteName = "skupper-router-console"
	}
	ensureServiceAccount("skupper", dep, oauthRouteName, kube)
	ensureViewRole(dep, kube)
	ensureRoleBinding("skupper", "skupper-view", dep, kube)


	ensureServiceForRouter("skupper-messaging", messagingServicePorts(), &owner, "", "", kube)
	if router.ConsoleAuthMode == ConsoleAuthModeInternal {
		//used for both skupper console and router console if enabled
		ensureSaslUsers(router.ConsoleUser, router.ConsolePassword, &owner, kube)
	}
	if router.ConsoleEnabled {
		servingCerts := ""
		termination := routev1.TLSTerminationEdge
		port := corev1.ServicePort{
			Name:       "console",
			Protocol:   "TCP",
			Port:       8080,
			TargetPort: intstr.FromInt(8080),
		}

		if router.ConsoleAuthMode == ConsoleAuthModeOpenshift {
			servingCerts = "skupper-proxy-certs"
			termination = routev1.TLSTerminationReencrypt
			port = corev1.ServicePort{
				Name:       "console",
				Protocol:   "TCP",
				Port:       443,
				TargetPort: intstr.FromInt(8443),
			}
		} else if router.ConsoleAuthMode == ConsoleAuthModeInternal {
			ensureSaslConfig(&owner, kube)
		}
		ensureServiceForRouter("skupper-router-console", []corev1.ServicePort{
			port,
		}, &owner, servingCerts, "", kube)
		if kube.Routes != nil {
			ensureRoute("skupper-router-console", "skupper-router-console", "console", termination, &owner, kube)
		} //else ??TODO?? create ingress
	}

	metricsPort := []corev1.ServicePort{
		corev1.ServicePort{
			Name:       "metrics",
			Protocol:   "TCP",
			Port:       8080,
			TargetPort: intstr.FromInt(8080),
		},
	}
	//TODO: make this configurable and secure
	if kube.Routes != nil {
		controllerServingCerts := ""
		termination := routev1.TLSTerminationEdge
		if router.ConsoleAuthMode == ConsoleAuthModeOpenshift {
			controllerServingCerts = "skupper-controller-certs"
			termination = routev1.TLSTerminationReencrypt
			metricsPort = []corev1.ServicePort{
				corev1.ServicePort{
					Name:       "metrics",
					Protocol:   "TCP",
					Port:       443,
					TargetPort: intstr.FromInt(8443),
				},
			}
		}
		ensureServiceForController("skupper-controller", metricsPort, &owner, controllerServingCerts, "", kube)
		if !clusterLocal {
			ensureRoute("skupper-controller", "skupper-controller", "metrics", termination, &owner, kube)
		}
	} else {
		if clusterLocal {
			ensureServiceForController("skupper-controller", metricsPort, &owner, "", "", kube)
		} else {
			ensureServiceForController("skupper-controller", metricsPort, &owner, "", "LoadBalancer", kube)
		}
	}

	return dep
}

func initProxyController(enableServiceSync bool, router *appsv1.Deployment, authConfig *Router, kube *KubeDetails) {
	oauthRouteName := ""
	if authConfig != nil && authConfig.ConsoleAuthMode == ConsoleAuthModeOpenshift {
		oauthRouteName = "skupper-controller"
	}
	ensureServiceAccount("skupper-proxy-controller", router, oauthRouteName, kube)
	ensureEditRole(router, kube)
	ensureRoleBinding("skupper-proxy-controller", "skupper-edit", router, kube)
	ensureProxyController(enableServiceSync, router, authConfig, kube)
}

func initEdge(router *Router, kube *KubeDetails, clusterLocal bool) *appsv1.Deployment {
	return initCommon(router, []string{"skupper-amqps"}, clusterLocal, kube)
}

func initInterior(router *Router, kube *KubeDetails, clusterLocal bool) *appsv1.Deployment {
	dep := initCommon(router, []string{"skupper-amqps", "skupper-internal"}, clusterLocal, kube)
	owner := get_owner_reference(dep)
	internalCa := ensureCA("skupper-internal-ca", &owner, kube)
	if clusterLocal {
		ensureServiceForRouter("skupper-internal", internalServicePorts(), &owner, "", "", kube)
		generateSecret(internalCa, "skupper-internal", "skupper-internal", "skupper-internal." + kube.Namespace, false, &owner, kube)
	} else if kube.Routes != nil {
		ensureServiceForRouter("skupper-internal", internalServicePorts(), &owner, "", "", kube)
		//TODO: handle loadbalancer service where routes are not available
		hosts := ensureRoute("skupper-inter-router", "skupper-internal", "inter-router", routev1.TLSTerminationPassthrough, &owner, kube)
		hosts += "," + ensureRoute("skupper-edge", "skupper-internal", "edge", routev1.TLSTerminationPassthrough, &owner, kube)
		generateSecret(internalCa, "skupper-internal", router.Name, hosts, false, &owner, kube)
	} else {
		service, err := ensureServiceForRouter("skupper-internal", internalServicePorts(), &owner, "", "LoadBalancer", kube)
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

func check_connection(name string, kube *KubeDetails) bool {
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Println("Skupper is not installed in '" + kube.Namespace + "'.  Use 'skupper init' to install.")
		} else {
			fmt.Println(err)
		}
		return false
	}
	mode := get_router_mode(current)
	var connectors []Connector
	if name == "all" {
		connectors = retrieveConnectors(mode, kube)
	} else {
		connector, err := get_connector(name, mode, kube)
		if err == nil {
			connectors = append(connectors, connector)
		} else {
			fmt.Printf("Could not find connector %s: %s", name, err)
			fmt.Println()
			return false
		}
	}
	connections, err := router.GetConnections(kube.Namespace, kube.Standard, kube.RestConfig)
	if err == nil {
		result := true
		for _, connector := range connectors {
			connection := router.GetInterRouterOrEdgeConnection(connector.Host + ":" + connector.Port, connections)
			if connection == nil || !connection.Active {
				fmt.Printf("Connection for %s not active", connector.Name)
				fmt.Println()
				result = false
			} else {
				fmt.Printf("Connection for %s is active", connector.Name)
				fmt.Println()
			}
		}
		return result
	} else {
		fmt.Printf("Could not check connections: %s", err)
		fmt.Println()
		return false
	}
}

func retrieveConnectors(mode RouterMode, kube *KubeDetails) []Connector {
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

func listConnectors(kube *KubeDetails) {
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		mode := get_router_mode(current)
		connectors := retrieveConnectors(mode, kube)
		if len(connectors) == 0 {
			fmt.Println("There are no connectors defined.")
		} else {
			fmt.Println("Connectors:")
			for _, c := range connectors {
				fmt.Printf("    %s:%s (name=%s)", c.Host, c.Port, c.Name)
				fmt.Println()
			}
		}
	} else if errors.IsNotFound(err) {
		fmt.Println("Skupper not installed in '" + kube.Namespace + "'")
	} else {
		log.Fatal(err)
	}
}

func get_connector(name string, mode RouterMode, kube *KubeDetails) (Connector, error) {
	s, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Get(name, metav1.GetOptions{})
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
		connector := Connector{
			Name: s.ObjectMeta.Name,
			Host: s.ObjectMeta.Annotations[hostKey],
			Port: s.ObjectMeta.Annotations[portKey],
			Role: role,
		}
		return connector, nil
	} else {
		log.Fatal("Could not retrieve connection-token secret:", name, err)
		return Connector{}, err
	}
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

func status(kube *KubeDetails) {
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		mode := get_router_mode(current)
		var modedesc string
		if mode == RouterModeEdge {
			modedesc = " in edge mode"
		}
		if current.Status.ReadyReplicas == 0 {
			fmt.Printf("Skupper is installed in namespace '%q%s'. Status pending...", kube.Namespace, modedesc)
		} else {
			connected, err := router.GetConnectedSites(mode == RouterModeEdge, kube.Namespace, kube.Standard, kube.RestConfig)
			for i :=0; i < 5 && err != nil; i++ {
				time.Sleep(500*time.Millisecond)
				connected, err = router.GetConnectedSites(mode == RouterModeEdge, kube.Namespace, kube.Standard, kube.RestConfig)
			}
			if err != nil {
				log.Fatalf("Skupper is enabled for namespace '%s'. Unable to determine connectivity:%s\n", kube.Namespace, err)
			} else {
				fmt.Printf("Skupper is enabled for namespace '%q%s'.", kube.Namespace, modedesc)
                                // Consider newlines between these sentences
				if connected.Total == 0 {
					fmt.Printf(" It is not connected to any other sites.")
				} else if connected.Total == 1 {
					fmt.Printf(" It is connected to 1 other site.")
				} else if connected.Total == connected.Direct {
					fmt.Printf(" It is connected to %d other sites.", connected.Total)
				} else {
					fmt.Printf(" It is connected to %d other sites (%d indirectly).", connected.Total, connected.Indirect)
				}
			}
			exposed := countServiceDefinitions(kube)
			if exposed == 1 {
				fmt.Printf(" 1 service is exposed.")
			} else if exposed > 0 {
				fmt.Printf(" %d services are exposed.", exposed)
			}
		}
		fmt.Println()
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
		found, connectors := remove_connector(name, retrieveConnectors(mode, kube))
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

func connect(secretFile string, connectorName string, cost int, kube *KubeDetails) {
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
				Cost: cost,
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
				fmt.Printf("Skupper is now configured to connect to %s:%s (name=%s)", connector.Host, connector.Port, connector.Name)
				fmt.Println()
			}
		} else if errors.IsAlreadyExists(err) {
			fmt.Println("A connector secret of that name already exist. Please choose a different name.")
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

func deleteSkupper(kube *KubeDetails) {
	err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Delete("skupper-site", &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Skupper is now removed from '" + kube.Namespace + "'.")
	} else if errors.IsNotFound(err) {
		err := kube.Standard.AppsV1().Deployments(kube.Namespace).Delete("skupper-router", &metav1.DeleteOptions{})
		if err == nil  {
			fmt.Println("Skupper is now removed from '" + kube.Namespace + "'.")
		} else if errors.IsNotFound(err) {
			fmt.Println("Skupper not installed in '" + kube.Namespace + "'.")
		} else {
			fmt.Println("Error while trying to delete:", err.Error())
		}
	} else {
		fmt.Println("Error while trying to delete:", err.Error())
	}
}

func ensureSkupperConfig(router *Router, kube *KubeDetails) (*corev1.ConfigMap, error) {
	config, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-site", metav1.GetOptions{})
	if err != nil  {
		if errors.IsNotFound(err) {
			siteid := randomId(10)
			configMap := corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "skupper-site",
				},
				Data: map[string]string{
					"id": siteid,
					"name": router.Name,
				},
			}
			created, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Create(&configMap)
			if err != nil {
				log.Fatal("Failed to create skupper-site config map: ", err.Error())
				return nil, err
			} else {
				return created, nil
			}
		} else {
			log.Fatal("Failed to retrieve skupper-site config:", err)
			return nil, err
		}
	} else {
		return config, nil
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

func exposeTarget() func(*cobra.Command,[]string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < 1  || (!strings.Contains(args[0], "/") && len(args) < 2) {
			return fmt.Errorf("expose target and name must be specified (e.g. 'skupper expose deployment <name>'")
		}
		if len(args) > 2 {
			return fmt.Errorf("illegal argument: %s", args[2])
		}
		if len(args) > 1 && strings.Contains(args[0], "/") {
			return fmt.Errorf("extra argument: %s", args[1])
		}
		targetType := args[0]
		if strings.Contains(args[0], "/") {
			parts := strings.Split(args[0], "/")
			targetType = parts[0]
		}
		if targetType != "deployment" && targetType != "statefulset" && targetType != "pods" {
			return fmt.Errorf("expose target type must be one of 'deployment', 'statefulset' or 'pods'")
		}
		return nil
	}
}

func createServiceArgs() func(*cobra.Command,[]string) error {
	return func(cmd *cobra.Command, args []string) error {
		if (len(args) < 1 || (!strings.Contains(args[0], ":") && len(args) < 2)) {
			return fmt.Errorf("Name and port must be specified")
		}
		if len(args) > 2 {
			return fmt.Errorf("illegal argument: %s", args[2])
		}
		if len(args) > 1 && strings.Contains(args[0], ":") {
			return fmt.Errorf("extra argument: %s", args[1])
		}
		return nil
	}
}

func deleteServiceArgs() func(*cobra.Command,[]string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("name of service to delete must be specified")
		} else if len(args) > 1 {
			return fmt.Errorf("illegal argument: %s", args[1])
		}
		return nil
	}
}

func bindArgs() func(*cobra.Command,[]string) error {
	return func(cmd *cobra.Command, args []string) error {
		if (len(args) < 2 || (!strings.Contains(args[1], "/") && len(args) < 3)) {
			return fmt.Errorf("Service name, target type and target name must all be specified (e.g. 'skupper bind <service-name> <target-type> <target-name>')")
		}
		if len(args) > 3 {
			return fmt.Errorf("illegal argument: %s", args[3])
		}
		if len(args) > 2 && strings.Contains(args[1], "/") {
			return fmt.Errorf("extra argument: %s", args[2])
		}
		return nil
	}
}

type ExposeOptions struct {
	Protocol      string
	Address       string
	Port          int
	TargetPort    int
	Headless      bool
	Aggregate     string
}

type Service struct {
	Address        string `json:"address"`
	Protocol       string `json:"protocol"`
	Port           int    `json:"port"`
	Headless       *Headless `json:"headless,omitempty"`
	EventChannel   bool `json:"eventchannel,omitempty"`
	Aggregate      string `json:"aggregate,omitempty"`
	Targets        []ServiceTarget `json:"targets,omitempty"`
}

type ServiceTarget struct {
	Name           string `json:"name,omitempty"`
	Selector       string `json:"selector"`
	TargetPort     int    `json:"targetPort,omitempty"`
}

type Headless struct {
	Name           string `json:"name"`
	Size           int    `json:"size"`
	TargetPort     int    `json:"targetPort,omitempty"`
}

func updateServiceDefinition(serviceName string, targetName string, selector string, port int, options ExposeOptions, owner *metav1.OwnerReference, kube *KubeDetails) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		//is the service already defined?
		serviceTarget := ServiceTarget {
			Selector: selector,
		}
		if targetName != "" {
			serviceTarget.Name = targetName
		}
		if options.TargetPort != 0 {
			serviceTarget.TargetPort = options.TargetPort
		}
		if current.Data == nil {
			current.Data = make(map[string]string)
		}
		jsonDef := current.Data[serviceName]
		if jsonDef == "" {
                        // "entry" seems a bit vague to me here
			fmt.Printf("Created new entry for service %s", serviceName)
			fmt.Println()
			serviceDef := Service{
				Address: serviceName,
				Protocol: options.Protocol,
				Port: port,
				Targets: []ServiceTarget {
					serviceTarget,
				},
			}
			if options.Aggregate != "" {
				serviceDef.Aggregate = options.Aggregate
			}
			encoded, err := jsonencoding.Marshal(serviceDef)
			if err != nil {
				fmt.Printf("Failed to create json for service definition: %s", err)
				fmt.Println()
				return
			} else {
				current.Data[serviceName] = string(encoded)
			}
		} else {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(jsonDef), &service)
			if err != nil {
				fmt.Printf("Failed to read json for service definition %s: %s", serviceName, err)
				fmt.Println()
				return
			} else if service.Headless != nil {
				fmt.Printf("Service %s already defined as headless. To allow target use skupper unexpose or skupper delete service.", serviceName)
				fmt.Println()
				return
			} else if service.Aggregate != options.Aggregate {
				fmt.Printf("Service %s already defined with aggregate=%s. To redefine, first use skupper unexpose or skupper delete service.", serviceName, service.Aggregate)
				fmt.Println()
				return
			} else {
				if options.TargetPort != 0 {
					serviceTarget.TargetPort = options.TargetPort
				} else if port != service.Port {
					serviceTarget.TargetPort = port
				}
				modified := false
				targets := []ServiceTarget{}
				for _, t := range service.Targets {
					if t.Name == serviceTarget.Name {
						modified = true
						targets =append(targets, serviceTarget)
						fmt.Printf("Updated target %s for service %s", serviceTarget.Name, serviceName)
						fmt.Println()
					} else {
						targets =append(targets, t)
					}
				}
				if !modified {
					targets = append(targets, serviceTarget)
					fmt.Printf("Added new target %s for service %s", serviceTarget.Name, serviceName)
					fmt.Println()
				}
				service.Targets = targets
				encoded, err := jsonencoding.Marshal(service)
				if err != nil {
					fmt.Printf("Failed to create json for service definition: %s", err)
					fmt.Println()
					return
				} else {
					current.Data[serviceName] = string(encoded)
				}
			}
		}
		_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
		if err != nil {
			log.Fatal("Failed to update skupper-services config map: ", err.Error())
		}
	} else if errors.IsNotFound(err) {
		serviceTarget := ServiceTarget {
			Selector: selector,
		}
		if targetName != "" {
			serviceTarget.Name = targetName
		}
		if options.TargetPort != 0 {
			serviceTarget.TargetPort = options.TargetPort
		}
		serviceDef := Service{
			Address: serviceName,
			Protocol: options.Protocol,
			Port: port,
			Targets: []ServiceTarget {
				serviceTarget,
			},
		}
		jsonDef, err := jsonencoding.Marshal(serviceDef)
		//need to create the configmap
		configMap := corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "skupper-services",
			},
			Data: map[string]string{
				serviceName: string(jsonDef),
			},
		}

		if owner != nil {
			configMap.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}
		}

		_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Create(&configMap)
		if err != nil {
			log.Fatal("Failed to create skupper-services config map: ", err.Error())
		}
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
	}

}

func updateHeadlessServiceDefinition(serviceName string, headless Headless, port int, options ExposeOptions, owner *metav1.OwnerReference, kube *KubeDetails) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		//is the service already defined?
		jsonDef := current.Data[serviceName]
		if jsonDef == "" {
			fmt.Printf("New entry for headless service %s", serviceName)
			fmt.Println()
			serviceDef := Service {
				Address: serviceName,
				Protocol: options.Protocol,
				Port: port,
				Headless: &headless,
			}
			encoded, err := jsonencoding.Marshal(serviceDef)
			if err != nil {
				fmt.Printf("Failed to create json for service definition: %s", err)
				fmt.Println()
				return
			} else {
				current.Data[serviceName] = string(encoded)
			}
		} else {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(jsonDef), &service)
			if err != nil {
				fmt.Printf("Failed to read json for service definition %s: %s", serviceName, err)
				fmt.Println()
				return
			} else {
				if len(service.Targets) > 0 {
					fmt.Printf("Non-headless service definition already exists for %s; unexpose first", serviceName)
					fmt.Println()
					return
				}
				service.Address = serviceName
				service.Protocol = options.Protocol
				service.Port = port
				service.Headless = &headless

				encoded, err := jsonencoding.Marshal(service)
				if err != nil {
					fmt.Printf("Failed to create json for service definition: %s", err)
					fmt.Println()
					return
				} else {
					current.Data[serviceName] = string(encoded)
				}
			}
		}
		_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
		if err != nil {
			log.Fatal("Failed to update skupper-services config map: ", err.Error())
		}
	} else if errors.IsNotFound(err) {
		serviceDef := Service{
			Address: serviceName,
			Protocol: options.Protocol,
			Port: port,
			Headless: &headless,
		}
		jsonDef, err := jsonencoding.Marshal(serviceDef)
		//need to create the configmap
		configMap := corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "skupper-services",
			},
			Data: map[string]string{
				serviceName: string(jsonDef),
			},
		}

		if owner != nil {
			configMap.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				*owner,
			}
		}

		_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Create(&configMap)
		if err != nil {
			log.Fatal("Failed to create skupper-services config map: ", err.Error())
		}
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
	}

}


func stringifySelector(labels map[string]string) string {
	result := ""
	for k, v := range labels {
		if result != "" {
			result += ","
		}
		result += k
		result += "="
		result += v
	}
	return result
}

func expose(targetType string, targetName string, options ExposeOptions, kube *KubeDetails) {
	router, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Println("Skupper is not enabled for '" + kube.Namespace + "'")
		} else {
			fmt.Println(err)
		}
	} else if options.Headless && targetType != "statefulset" {
		fmt.Println("The headless option is only supported for statefulsets")
	} else if options.Headless && options.Aggregate != "" {
		fmt.Println("The headless option and aggregate option are not compatible")
	} else {
		owner := get_owner_reference(router)
		if targetType == "deployment" {
			target, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get(targetName, metav1.GetOptions{})
			if err == nil  {
				//TODO: handle case where there is more than one container (need --container option?)
				port := options.Port
				targetPort := options.TargetPort
				if target.Spec.Template.Spec.Containers[0].Ports != nil {
					if port == 0 {
						port = int(target.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
					} else if targetPort == 0 {
						targetPort = int(target.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
					}
				}
				if port == 0 {
					fmt.Printf("Container in deployment does not specify port, use --port option to provide it")
					fmt.Println()
				} else {
					selector := stringifySelector(target.Spec.Selector.MatchLabels)
					if options.Address == "" {
						updateServiceDefinition(target.ObjectMeta.Name, "", selector, port, options, &owner, kube)
					} else {
						updateServiceDefinition(options.Address, target.ObjectMeta.Name, selector, port, options, &owner, kube)
					}
				}
			} else {
				fmt.Printf("Could not read deployment %s: %s", targetName, err)
				fmt.Println()
			}
		} else if targetType == "statefulset" {
			if options.Headless {
				statefulset, err := kube.Standard.AppsV1().StatefulSets(kube.Namespace).Get(targetName, metav1.GetOptions{})
				if err == nil  {
					if options.Address != "" && options.Address != statefulset.Spec.ServiceName {
						fmt.Printf("Cannot specify different address from service name for headless service.")
						fmt.Println()
					}
					service, err := kube.Standard.CoreV1().Services(kube.Namespace).Get(statefulset.Spec.ServiceName, metav1.GetOptions{})
					if err == nil  {
						var port int
						var headless Headless
						if options.Port != 0 {
							port = options.Port
						} else if len(service.Spec.Ports) == 1 {
							port = int(service.Spec.Ports[0].Port)
							if service.Spec.Ports[0].TargetPort.IntValue() != 0 && int(service.Spec.Ports[0].Port) != service.Spec.Ports[0].TargetPort.IntValue() {
								//TODO: handle string ports
								headless.TargetPort = service.Spec.Ports[0].TargetPort.IntValue()
							}
						} else {
							fmt.Printf("Service %s has multiple ports, specify which to use with --port", statefulset.Spec.ServiceName)
							fmt.Println()
						}
						if port > 0 {
							headless.Name = statefulset.ObjectMeta.Name
							headless.Size = int(*statefulset.Spec.Replicas)
							updateHeadlessServiceDefinition(service.ObjectMeta.Name, headless, port, options, &owner, kube)
						}
					} else if errors.IsNotFound(err) {
						fmt.Printf("Service %s not found for statefulset %s", statefulset.Spec.ServiceName, targetName)
						fmt.Println()
					} else {
						fmt.Printf("Could not read service %s: %s", statefulset.Spec.ServiceName, err)
						fmt.Println()
					}
				} else if errors.IsNotFound(err) {
					fmt.Printf("StatefulSet %s not found", targetName)
					fmt.Println()
				} else {
					fmt.Printf("Could not read StatefulSet %s: %s", targetName, err)
					fmt.Println()
				}
			} else {
				target, err := kube.Standard.AppsV1().StatefulSets(kube.Namespace).Get(targetName, metav1.GetOptions{})
				if err == nil  {
					//TODO: handle case where there is more than one container (need --container option?)
					port := options.Port
					targetPort := options.TargetPort
					if target.Spec.Template.Spec.Containers[0].Ports != nil {
						if port == 0 {
							port = int(target.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
						} else if targetPort == 0 {
							targetPort = int(target.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
						}
					}
					if port == 0 {
						fmt.Printf("Container in statefulset does not specify port, use --port option to provide it")
						fmt.Println()
					} else {
						selector := stringifySelector(target.Spec.Selector.MatchLabels)
						if options.Address == "" {
							updateServiceDefinition(target.ObjectMeta.Name, "", selector, port, options, &owner, kube)
						} else {
							updateServiceDefinition(options.Address, target.ObjectMeta.Name, selector, port, options, &owner, kube)
						}
					}
				} else {
					fmt.Printf("Could not read statefulset %s: %s", targetName, err)
					fmt.Println()
				}
			}
		} else if targetType == "pods" {
			fmt.Println("Not yet implemented")
		} else {
			fmt.Println("Unsupported target type", targetType)
		}
	}
}

func removeServiceTarget(serviceName string, targetName string, deleteServiceIfNoTargets bool, kube *KubeDetails) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		jsonDef := current.Data[serviceName]
		if jsonDef == "" {
			fmt.Printf("Could not find entry for service %s", serviceName)
			fmt.Println()
		} else {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(jsonDef), &service)
			if err != nil {
				fmt.Printf("Failed to read json for service definition %s: %s", serviceName, err)
				fmt.Println()
				return
			} else {
				modified := false
				targets := []ServiceTarget{}
				for _, t := range service.Targets {
					if t.Name == targetName || (t.Name == "" && targetName == serviceName) {
						modified = true
					} else {
						targets = append(targets, t)
					}
				}
				if !modified {
					fmt.Printf("Could not find target %s for service %s", targetName, serviceName)
					fmt.Println()
					return
				}
				if len(targets) == 0 && deleteServiceIfNoTargets {
					fmt.Printf("Removing service definition for %s", serviceName)
					fmt.Println()
					delete(current.Data, serviceName)
				} else {
					service.Targets = targets
					encoded, err := jsonencoding.Marshal(service)
					if err != nil {
						fmt.Printf("Failed to create json for service definition: %s", err)
						fmt.Println()
						return
					} else {
						fmt.Printf("Removing target %s from service %s", targetName, serviceName)
						fmt.Println()
						current.Data[serviceName] = string(encoded)
					}
				}
			}
		}
		_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
		if err != nil {
			log.Fatal("Failed to update skupper-services config map: ", err.Error())
		}
	} else if errors.IsNotFound(err) {
		log.Fatal("No skupper services defined: ", err.Error())
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
	}

}

func unexpose(targetType string, targetName string, address string, kube *KubeDetails) {
	if targetType == "deployment" || targetType == "statefulset" {
		if address == "" {
			removeServiceTarget(targetName, targetName, true, kube)
		} else {
			removeServiceTarget(address, targetName, true, kube)
		}
	} else if targetType == "pods" {
		fmt.Println("Not yet implemented")
	} else {
		fmt.Println("Unsupported target type", targetType)
	}
}

func listServiceDefinitions(kube *KubeDetails) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		fmt.Println("Services exposed through Skupper:")
		for k, v := range current.Data {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(v), &service)
			if err != nil {
				fmt.Printf("Failed to parse json for service definition %s: %s", k, err)
				fmt.Println()
			} else if len(service.Targets) == 0 {
				fmt.Printf("    %s (%s port %d)", service.Address, service.Protocol, service.Port)
				if service.EventChannel {
					fmt.Printf(", event-channel")
				} else if service.Aggregate != "" {
					fmt.Printf(", aggregate=%s", service.Aggregate)
				}
				fmt.Println()
			} else {
				if service.EventChannel {
					fmt.Printf("    %s (%s port %d), event-channel with subscriptions", service.Address, service.Protocol, service.Port)
				} else if service.Aggregate != "" {
					fmt.Printf("    %s (%s port %d), aggregate=%s, with targets", service.Address, service.Protocol, service.Port, service.Aggregate)
				} else {
					fmt.Printf("    %s (%s port %d) with targets", service.Address, service.Protocol, service.Port)
				}
				fmt.Println()
				for _, t := range service.Targets {
					var name string
					if t.Name != "" {
						name = fmt.Sprintf("name=%s", t.Name)
					}
					fmt.Printf("      => %s %s", t.Selector, name)
					fmt.Println()
				}
			}
		}
	} else if errors.IsNotFound(err) {
		fmt.Println("No services defined")
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
	}
}

func saveServiceDefinition(service *Service, kube *KubeDetails) {
	encoded, err := jsonencoding.Marshal(service)
	if err != nil {
		fmt.Printf("Failed to encode service definition as json: %s", err)
		fmt.Println()
	} else {
		current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
		if err != nil {
			log.Fatal("Failed to get skupper-services config map: ", err.Error())
		} else {
			current.Data[service.Address] = string(encoded)
			_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
			if err != nil {
				log.Fatal("Failed to update skupper-services config map: ", err.Error())
			}
		}
	}
}

func getServiceDefinition(serviceName string, kube *KubeDetails) (*Service, error) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		jsonDef := current.Data[serviceName]
		if jsonDef == "" {
			return nil, nil
		} else {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(jsonDef), &service)
			if err != nil {
				fmt.Printf("Failed to read json for service definition %s: %s", serviceName, err)
				fmt.Println()
				return nil, err
			} else {
				return &service, nil
			}
		}
	} else if errors.IsNotFound(err) {
		return nil, nil
	} else {
		fmt.Printf("Failed to retrieve service definitions: %s", err)
		fmt.Println()
		return nil, err
	}
}

func addTargetToService(service *Service, target *ServiceTarget) {
	modified := false
	targets := []ServiceTarget{}
	for _, t := range service.Targets {
		if t.Name == target.Name {
			modified = true
			targets = append(targets, *target)
			fmt.Printf("Updated target %s for service %s", target.Name, service.Address)
			fmt.Println()
		} else {
			targets = append(targets, t)
		}
	}
	if !modified {
		targets = append(targets, *target)
		fmt.Printf("Added new target %s for service %s", target.Name, service.Address)
		fmt.Println()
	}
	service.Targets = targets
}

func bindServiceTarget(serviceName string, targetType string, targetName string, targetPort int, protocol string, kube *KubeDetails) {
	service, err := getServiceDefinition(serviceName, kube)
	if service == nil {
		if err == nil {
			fmt.Printf("Service not defined: %s", serviceName)
			fmt.Println()
		}
	} else {
		if targetType == "deployment" {
			target, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get(targetName, metav1.GetOptions{})
			if err == nil  {
				serviceTarget := ServiceTarget {
					Selector: stringifySelector(target.Spec.Selector.MatchLabels),
					Name: target.ObjectMeta.Name,
				}
				if targetPort != 0 {
					serviceTarget.TargetPort = targetPort
				}
				addTargetToService(service, &serviceTarget)
				saveServiceDefinition(service, kube)
			} else {
				fmt.Printf("Could not read deployment %s: %s", targetName, err)
				fmt.Println()
			}
		} else if targetType == "statefulset" {
			if service.Headless != nil {
				fmt.Printf("Cannot bind additional targets to a headless.")
				fmt.Println()
			} else {
				target, err := kube.Standard.AppsV1().StatefulSets(kube.Namespace).Get(targetName, metav1.GetOptions{})
				if err == nil  {
					serviceTarget := ServiceTarget {
						Selector: stringifySelector(target.Spec.Selector.MatchLabels),
						Name: target.ObjectMeta.Name,
					}
					if targetPort != 0 {
						serviceTarget.TargetPort = targetPort
					}
					addTargetToService(service, &serviceTarget)
					saveServiceDefinition(service, kube)
 				} else {
					fmt.Printf("Could not read statefulset %s: %s", targetName, err)
					fmt.Println()
				}
			}
		} else if targetType == "pods" {
			fmt.Println("Not yet implemented")
		} else {
			fmt.Println("Unsupported target type", targetType)
		}
	}
}

func unbindServiceTarget(serviceName string, targetType string, targetName string, kube *KubeDetails) {
	if targetType == "deployment" || targetType == "statefulset" {
		removeServiceTarget(serviceName, targetName, false, kube)
	} else if targetType == "pods" {
		fmt.Println("Not yet implemented")
	} else {
		fmt.Println("Unsupported target type", targetType)
	}
}

func createServiceDefinition(serviceName string, port int, mapping string, aggregate string, eventChannel bool, kube *KubeDetails) {
	router, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Println("Skupper is not enabled for '" + kube.Namespace + "'")
		} else {
			fmt.Println(err)
		}
	} else {
		serviceDef := Service{
			Address: serviceName,
			Protocol: mapping,
			Port: port,
			Targets: []ServiceTarget {},
		}
		if eventChannel {
			serviceDef.EventChannel = true
		}
		if aggregate != "" {
			serviceDef.Aggregate = aggregate
		}
		current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
		if err == nil  {
			if current.Data == nil {
				current.Data = make(map[string]string)
			}
			jsonDef := current.Data[serviceName]
			if jsonDef == "" {
				// "entry" seems a bit vague to me here
				fmt.Printf("Created new entry for service %s", serviceName)
				fmt.Println()
				encoded, err := jsonencoding.Marshal(serviceDef)
				if err != nil {
					fmt.Printf("Failed to create json for service definition: %s", err)
					fmt.Println()
					return
				} else {
					current.Data[serviceName] = string(encoded)
				}
				_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
				if err != nil {
					log.Fatal("Failed to update skupper-services config map: ", err.Error())
				}
			} else {
				fmt.Printf("Service %s already defined", serviceName)
				fmt.Println()
				return
			}
		} else if errors.IsNotFound(err) {
			jsonDef, err := jsonencoding.Marshal(serviceDef)
			//need to create the configmap
			configMap := corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "skupper-services",
				},
				Data: map[string]string{
					serviceName: string(jsonDef),
				},
			}
			configMap.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				get_owner_reference(router),
			}

			_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Create(&configMap)
			if err != nil {
				log.Fatal("Failed to create skupper-services config map: ", err.Error())
			}
		} else {
			fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
		}
	}
}

func deleteServiceDefinition(serviceName string, kube *KubeDetails) {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		jsonDef := current.Data[serviceName]
		if jsonDef == "" {
			fmt.Printf("Could not find service %s", serviceName)
			fmt.Println()
		} else {
			fmt.Printf("Removing service definition for %s", serviceName)
			fmt.Println()
			delete(current.Data, serviceName)
			_, err = kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Update(current)
			if err != nil {
				log.Fatal("Failed to update skupper-services config map: ", err.Error())
			}
		}
	} else if errors.IsNotFound(err) {
		log.Fatal("No skupper services defined: ", err.Error())
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
	}
}

func countServiceDefinitions(kube *KubeDetails) int {
	current, err := kube.Standard.CoreV1().ConfigMaps(kube.Namespace).Get("skupper-services", metav1.GetOptions{})
	if err == nil  {
		count := 0
		for k, v := range current.Data {
			service := Service {}
			err = jsonencoding.Unmarshal([]byte(v), &service)
			if err != nil {
				fmt.Printf("Invalid service definition %s: %s", k, err)
				fmt.Println()
			} else {
				count = count + 1
			}
		}
		return count
	} else if errors.IsNotFound(err) {
		return 0
	} else {
		fmt.Println("Could not retrieve service definitions from configmap 'skupper-services'", err)
		return 0
	}
}

type KubeDetails struct {
	Namespace string
	Standard *kubernetes.Clientset
	Routes *routev1client.RouteV1Client
	RestConfig *restclient.Config
}

type KubeOptions struct {
	namespace string
	context string
	kubeconfig string
}

func initKubeConfig(options *KubeOptions) *KubeDetails {
	details := KubeDetails{}

	config := clientcmd.NewDefaultClientConfigLoadingRules()
	if options.kubeconfig != "" {
		config = &clientcmd.ClientConfigLoadingRules{ExplicitPath: options.kubeconfig}
	}
        kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
                config,
                &clientcmd.ConfigOverrides{
			CurrentContext:options.context,
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

	if options.namespace == "" {
		details.Namespace, _, err = kubeconfig.Namespace()
		if err != nil {
			log.Fatal(err)
		}
	} else {
		details.Namespace = options.namespace
	}

	return &details
}

func main() {
	routev1.AddToScheme(scheme.Scheme)
	routev1.AddToSchemeInCoreGroup(scheme.Scheme)

	kubeoptions := KubeOptions{}

	var skupperName string
	var isEdge bool
	var enableProxyController bool
	var enableServiceSync bool
	var enableRouterConsole bool
	var enableConsole bool
	var consoleAuthMode string
	var consoleUser string
	var consolePassword string
	var clusterLocal bool
	var cmdInit = &cobra.Command{
		Use:   "init",
		Short: "Initialise a Skupper site",
		Long: `init sets up a router and other supporting objects to provide a functional Skupper installation that can then be connected to other Skupper sites`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			router := Router{
				Name: skupperName,
				Mode: RouterModeInterior,
				Replicas: 1,
			}
			router.ConsoleEnabled = enableRouterConsole
			if enableRouterConsole || enableConsole {
				if consoleAuthMode == string(ConsoleAuthModeInternal) || consoleAuthMode == "" {
					router.ConsoleAuthMode = ConsoleAuthModeInternal
					router.ConsoleUser = consoleUser
					router.ConsolePassword = consolePassword
					if router.ConsoleUser == "" {
						router.ConsoleUser = "admin"
					}
					if router.ConsolePassword == "" {
						router.ConsolePassword = randomId(10)
					}
				} else {
					if consoleUser != "" {
						log.Fatal("--console-user only valid when --console-auth=internal")
					}
					if consolePassword != "" {
						log.Fatal("--console-password only valid when --console-auth=internal")
					}
					if consoleAuthMode == string(ConsoleAuthModeOpenshift) {
						router.ConsoleAuthMode = ConsoleAuthModeOpenshift
					} else if consoleAuthMode == string(ConsoleAuthModeUnsecured) {
						router.ConsoleAuthMode = ConsoleAuthModeUnsecured
					} else {
						log.Fatal("Unrecognised console authentication mode: ", consoleAuthMode)
					}
				}
			}

			kube := initKubeConfig(&kubeoptions)
			var dep *appsv1.Deployment
			if !isEdge {
				dep = initInterior(&router, kube, clusterLocal)
			} else {
				router.Mode = RouterModeEdge
				dep = initEdge(&router, kube, clusterLocal)
			}
			if enableProxyController {
				var authConfig *Router
				if enableConsole {
					authConfig = &router
				}
				initProxyController(enableServiceSync, dep, authConfig, kube)
			}
			fmt.Println("Skupper is now installed in namespace '" + kube.Namespace + "'.  Use 'skupper status' to get more information.")
		},
	}
	cmdInit.Flags().StringVarP(&skupperName, "site-name", "", "", "Provide a specific name for this skupper installation")
	cmdInit.Flags().BoolVarP(&isEdge, "edge", "", false, "Configure as an edge")
	cmdInit.Flags().BoolVarP(&enableProxyController, "enable-proxy-controller", "", true, "Setup the proxy controller as well as the router")
	cmdInit.Flags().BoolVarP(&enableServiceSync, "enable-service-sync", "", true, "Configure proxy controller to particiapte in service sync (not relevant if --enable-proxy-controller is false)")
	cmdInit.Flags().BoolVarP(&enableRouterConsole, "enable-router-console", "", false, "Enable router console")
	cmdInit.Flags().BoolVarP(&enableConsole, "enable-console", "", true, "Enable skupper console")
	cmdInit.Flags().StringVarP(&consoleAuthMode, "console-auth", "", "", "Authentication mode for console(s). One of: 'openshift', 'internal', 'unsecured'")
	cmdInit.Flags().StringVarP(&consoleUser, "console-user", "", "", "Router console user. Valid only when --router-console-auth=internal")
	cmdInit.Flags().StringVarP(&consolePassword, "console-password", "", "", "Router console user. Valid only when --router-console-auth=internal")
	cmdInit.Flags().BoolVarP(&clusterLocal, "cluster-local", "", false, "Set up skupper to only accept connections from within the local cluster.")

	var cmdDelete = &cobra.Command{
		Use:   "delete",
		Short: "Delete skupper installation",
		Long: `delete will delete any skupper related objects from the namespace`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			deleteSkupper(initKubeConfig(&kubeoptions))
		},
	}


	var clientIdentity string
	var cmdConnectionToken = &cobra.Command{
		Use:   "connection-token <output-file>",
		Short: "Create a connection token.  The 'connect' command uses the token to establish a connection from a remote Skupper site.",
		Args: requiredArg("output-file"),
		Run: func(cmd *cobra.Command, args []string) {
			generateConnectSecret(clientIdentity, args[0], initKubeConfig(&kubeoptions))
		},
	}
	cmdConnectionToken.Flags().StringVarP(&clientIdentity, "client-identity", "i", "skupper", "Provide a specific identity as which connecting skupper installation will be authenticated")

	var connectionName string
	var cost int
	var cmdConnect = &cobra.Command{
		Use:   "connect <connection-token-file>",
		Short: "Connect the current Skupper site to a remote Skupper site",
		Args: requiredArg("connection-token"),
		Run: func(cmd *cobra.Command, args []string) {
			connect(args[0], connectionName, cost, initKubeConfig(&kubeoptions))
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "connection-name", "", "", "Provide a specific name for the connection. The name is used when removing a connection with disconnect.")
	cmdConnect.Flags().IntVarP(&cost, "cost", "", 1, "Specify a cost for this connection.")

	var cmdDisconnect = &cobra.Command{
		Use:   "disconnect <name>",
		Short: "Remove the specified connection",
		Args: requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			disconnect(args[0], initKubeConfig(&kubeoptions))
		},
	}

	var waitFor int
	var cmdCheckConnection = &cobra.Command{
		Use:   "check-connection all|<connection-name>",
		Short: "Check whether a connection to another Skupper site is active",
		Args: requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			result := check_connection(args[0], initKubeConfig(&kubeoptions))
			for i := 0; !result && i < waitFor; i++ {
				time.Sleep(time.Second)
				result = check_connection(args[0], initKubeConfig(&kubeoptions))
			}
			if !result {
				os.Exit(-1)
			}
		},
	}
	cmdCheckConnection.Flags().IntVar(&waitFor, "wait", 0, "The number of seconds to wait for connections to become active")

	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "Report the status of the current Skupper site",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			status(initKubeConfig(&kubeoptions))
		},
	}

	var cmdListConnectors = &cobra.Command{
		Use:   "list-connectors",
		Short: "List configured outgoing connections",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			listConnectors(initKubeConfig(&kubeoptions))
		},
	}

	exposeOptions := ExposeOptions{}
	var cmdExpose = &cobra.Command{
		Use:   "expose [deployment <name>|pods <selector>|statefulset <statefulsetname>]",
		Short: "Expose a set of pods through a Skupper address",
		Args: exposeTarget(),
		Run: func(cmd *cobra.Command, args []string) {
			targetType := args[0]
			var targetName string
			if len(args) == 2 {
				targetName = args[1]
			} else {
				parts := strings.Split(args[0], "/")
				targetType = parts[0]
				targetName = parts[1]
			}
			expose(targetType, targetName, exposeOptions, initKubeConfig(&kubeoptions))
		},
	}
	cmdExpose.Flags().StringVar(&(exposeOptions.Protocol), "protocol", "tcp", "The protocol to proxy (tcp, http or http2)")
	cmdExpose.Flags().StringVar(&(exposeOptions.Address), "address", "", "The Skupper address to expose")
	cmdExpose.Flags().IntVar(&(exposeOptions.Port), "port", 0, "The port to expose on")
	cmdExpose.Flags().IntVar(&(exposeOptions.TargetPort), "target-port", 0, "The port to target on pods")
	cmdExpose.Flags().BoolVar(&(exposeOptions.Headless), "headless", false, "Expose through a headless service (valid only for a statefulset target)")
	cmdExpose.Flags().StringVar(&(exposeOptions.Aggregate), "aggregate", "", "Aggregation strategy (one of 'json' or 'multipart').")


	var unexposeAddress string
	var cmdUnexpose = &cobra.Command{
		Use:   "unexpose [deployment <name>|pods <selector>|statefulset <statefulsetname>]",
		Short: "Unexpose a set of pods previously exposed through a Skupper address",
		Args: exposeTarget(),
		Run: func(cmd *cobra.Command, args []string) {
			targetType := args[0]
			var targetName string
			if len(args) == 2 {
				targetName = args[1]
			} else {
				parts := strings.Split(args[0], "/")
				targetType = parts[0]
				targetName = parts[1]
			}
			unexpose(targetType, targetName, unexposeAddress, initKubeConfig(&kubeoptions))
		},
	}
	cmdUnexpose.Flags().StringVar(&unexposeAddress, "address", "", "Skupper address the target was exposed as")

	var cmdListExposed = &cobra.Command{
		Use:   "list-exposed",
		Short: "List services exposed over the Skupper network",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			listServiceDefinitions(initKubeConfig(&kubeoptions))
		},
	}


	var cmdService = &cobra.Command{
		Use:   "service create <name> <port> or service delete port",
		Short: "Manage skupper service definitions",
	}

	var mapping string
	var eventChannel bool
	var aggregate string
	var cmdCreateService = &cobra.Command{
		Use:   "create <name> <port>",
		Short: "Create a skupper service",
		Args: createServiceArgs(),
		Run: func(cmd *cobra.Command, args []string) {
			if aggregate != "" && eventChannel {
				fmt.Printf("Only one of aggregate and event-channel can be specified for a given service.")
				fmt.Println()
			} else if aggregate != "" && aggregate != "json" && aggregate != "multipart" {
				fmt.Printf("%s is not a valid aggregation strategy. Choose 'json' or 'multipart'.", aggregate)
				fmt.Println()
			} else if mapping != "" && mapping != "tcp" && mapping != "http" && mapping != "http2" {
				fmt.Printf("%s is not a valid mapping. Choose 'tcp', 'http' or 'http2'.", mapping)
				fmt.Println()
			} else if aggregate != "" && mapping != "http" {
				fmt.Printf("The aggregate option is currently only valid for --mapping http")
				fmt.Println()
			} else if eventChannel && mapping != "http" {
				fmt.Printf("The event-channel option is currently only valid for --mapping http")
				fmt.Println()
			} else {
				var serviceName string
				var sPort string
				if len(args) == 1 {
					parts := strings.Split(args[0], ":")
					serviceName = parts[0]
					sPort = parts[1]
				} else {
					serviceName = args[0]
					sPort = args[1]
				}
				servicePort, err := strconv.Atoi(sPort)
				if err != nil {
					fmt.Printf("%s is not a valid port.", sPort)
					fmt.Println()
				} else {
					createServiceDefinition(serviceName, servicePort, mapping, aggregate, eventChannel, initKubeConfig(&kubeoptions))
				}
			}
		},
	}
	cmdCreateService.Flags().StringVar(&mapping, "mapping", "tcp", "The mapping in use for this service address (currently one of tcp or http)")
	cmdCreateService.Flags().StringVar(&aggregate, "aggregate", "", "The aggregation strategy to use. One of 'json' or 'multipart'. If specified requests to this service will be sent to all registered implementations and the responses aggregated.")
	cmdCreateService.Flags().BoolVar(&eventChannel, "event-channel", false, "If specified, this service will be a channel for multicast events.")
	cmdService.AddCommand(cmdCreateService)

	var cmdDeleteService = &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a skupper service",
		Args: deleteServiceArgs(),
		Run: func(cmd *cobra.Command, args []string) {
			deleteServiceDefinition(args[0], initKubeConfig(&kubeoptions))
		},
	}
	cmdService.AddCommand(cmdDeleteService)

	var targetPort int
	var protocol string
	var cmdBind = &cobra.Command{
		Use:   "bind <service-name> <target-type> <target-name>",
		Short: "Bind a target to a service",
		Args: bindArgs(),
		Run: func(cmd *cobra.Command, args []string) {
			if protocol != "" && protocol != "tcp" && protocol != "http" && protocol != "http2" {
				fmt.Printf("%s is not a valid protocol. Choose 'tcp', 'http' or 'http2'.", protocol)
				fmt.Println()
			} else {
				var targetType string
				var targetName string
				if len(args) == 2 {
					parts := strings.Split(args[1], "/")
					targetType = parts[0]
					targetName = parts[1]
				} else if len(args) == 3 {
					targetType = args[1]
					targetName = args[2]
				}
				bindServiceTarget(args[0], targetType, targetName, targetPort, protocol, initKubeConfig(&kubeoptions))
			}
		},
	}
	cmdBind.Flags().StringVar(&mapping, "protocol", "tcp", "The protocol to proxy (tcp, http or http2.")
	cmdBind.Flags().IntVar(&targetPort, "target-port", 0, "The port the target is listening on.")

	var cmdUnbind = &cobra.Command{
		Use:   "unbind <service-name> <target-type> <target-name>",
		Short: "Unbind a target from a service",
		Args: bindArgs(),
		Run: func(cmd *cobra.Command, args []string) {
			var targetType string
			var targetName string
			if len(args) == 2 {
				parts := strings.Split(args[1], "/")
				targetType = parts[0]
				targetName = parts[1]
			} else if len(args) == 3 {
				targetType = args[1]
				targetName = args[2]
			}
			unbindServiceTarget(args[0], targetType, targetName, initKubeConfig(&kubeoptions))
		},
	}

	var cmdVersion = &cobra.Command{
		Use:   "version",
		Short: "Report the version of the Skupper CLI and services",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			kubeConfig := initKubeConfig(&kubeoptions)
			routerVersion := kube.GetComponentVersion(kubeConfig.Namespace, kubeConfig.Standard, "router")
			proxyControllerVersion := kube.GetComponentVersion(kubeConfig.Namespace, kubeConfig.Standard, "proxy-controller")
			fmt.Printf("client version           %s\n", version)
			fmt.Printf("router version           %s\n", routerVersion)
			fmt.Printf("controller version       %s\n", proxyControllerVersion)
		},
	}

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.Version = version
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdConnectionToken, cmdConnect, cmdDisconnect, cmdCheckConnection,
		cmdStatus, cmdListConnectors, cmdExpose, cmdUnexpose, cmdListExposed, cmdService,
		cmdBind, cmdUnbind, cmdVersion)
	rootCmd.PersistentFlags().StringVarP(&kubeoptions.kubeconfig, "kubeconfig", "", "", "Path to the kubeconfig file to use")
	rootCmd.PersistentFlags().StringVarP(&kubeoptions.context, "context", "c", "", "The kubeconfig context to use")
	rootCmd.PersistentFlags().StringVarP(&kubeoptions.namespace, "namespace", "n", "", "The Kubernetes namespace to use")
	rootCmd.Execute()
}
