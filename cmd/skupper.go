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
	"text/template"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
        "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
        "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/spf13/cobra"
	"github.com/skupperproject/skupper-cli/pkg/certs"
)

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
type Router struct {
	Name string
	Mode RouterMode
	Replicas int32
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

# TODO: secure console
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
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
    #TODO: fix this
    #verifyHostname: false
}

`
	var buff bytes.Buffer
	connectorconfig := template.Must(template.New("connectorconfig").Parse(config))
	connectorconfig.Execute(&buff, connector)
	return buff.String()
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
	ports = append(ports, corev1.ContainerPort{
		Name:          "http",
		ContainerPort: 8080,
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
					Port: intstr.FromInt(8080),
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
	return map[string]string{
		"application":    "skupper",
		"skupper-component": component,
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
						"prometheus.io/port":   "8080",
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
	return dep
}

func randomId(length int) string {
    buffer := make([]byte, length)
    rand.Read(buffer)
    result := base64.StdEncoding.EncodeToString(buffer)
    return result[:length]
}

func ensureProxyController(enableServiceSync bool, kube *KubeDetails) {
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
			log.Fatal("Failed to create router deployment: " + err.Error())
		}
	} else {
		log.Fatal("Failed to check router deployment: " + err.Error())
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

func ensureCA(name string, kube *KubeDetails) *corev1.Secret {
	existing, err :=kube.Standard.CoreV1().Secrets(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("CA", name, "already exists")
		return existing
	} else if errors.IsNotFound(err) {
		ca := certs.GenerateCASecret(name, name)
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

func ensureService(name string, ports []corev1.ServicePort, kube *KubeDetails) {
	_, err :=kube.Standard.CoreV1().Services(kube.Namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("Service", name, "already exists")
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
		_, err := kube.Standard.CoreV1().Services(kube.Namespace).Create(service)
		if err != nil {
			fmt.Println("Failed to create service", name, ": ", err.Error())
		}
	} else {
		fmt.Println("Failed while checking service", name, ": ", err.Error())
	}
}

func ensureRoute(name string, targetService string, targetPort string, kube *KubeDetails) string {
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
					Termination:                   routev1.TLSTerminationPassthrough,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyNone,
				},
			},
		}

		created, err := kube.Routes.Routes(kube.Namespace).Create(route)
		if err != nil {
			fmt.Println("Failed to create service", name, ": ", err.Error())
		} else {
			return created.Spec.Host
		}
	} else {
		fmt.Println("Failed while checking service", name, ": ", err.Error())
	}
	return ""
}

func generateSecret(caSecret *corev1.Secret, name string, subject string, hosts string, includeConnectJson bool, kube *KubeDetails) {
	secret := certs.GenerateSecret(name, subject, hosts, caSecret)
	if includeConnectJson {
		secret.Data["connect.json"] = []byte(connectJson())
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

func ensureServiceAccount(name string, router *appsv1.Deployment, kube *KubeDetails) *corev1.ServiceAccount {
	serviceaccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				metav1.OwnerReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       router.ObjectMeta.Name,
					UID:        router.ObjectMeta.UID,
				},
			},
		},
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
				metav1.OwnerReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       router.ObjectMeta.Name,
					UID:        router.ObjectMeta.UID,
				},
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
				metav1.OwnerReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       router.ObjectMeta.Name,
					UID:        router.ObjectMeta.UID,
				},
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
				metav1.OwnerReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       router.ObjectMeta.Name,
					UID:        router.ObjectMeta.UID,
				},
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

func initCommon(router *Router, volumes []string, kube *KubeDetails) *appsv1.Deployment {
	if router.Name == "" {
		router.Name = kube.Namespace
	}
	ca := ensureCA("skupper-ca", kube)
	generateSecret(ca, "skupper-amqps", "skupper-messaging", "skupper-messaging,skupper-messaging." + kube.Namespace + ".svc.cluster.local", false, kube)
	generateSecret(ca, "skupper", "skupper-messaging", "", true, kube)
	dep := ensureRouterDeployment(router, volumes, kube)
	ensureServiceAccount("skupper", dep, kube)
	ensureViewRole(dep, kube)
	ensureRoleBinding("skupper", "skupper-view", dep, kube)

	ensureService("skupper-messaging", messagingServicePorts(), kube)

	return dep
}

func initProxyController(enableServiceSync bool, router *appsv1.Deployment, kube *KubeDetails) {
	ensureServiceAccount("skupper-proxy-controller", router, kube)
	ensureEditRole(router, kube)
	ensureRoleBinding("skupper-proxy-controller", "skupper-edit", router, kube)
	ensureProxyController(enableServiceSync, kube)
}

func initEdge(name string, kube *KubeDetails) *appsv1.Deployment {
	router := Router{
		Name: name,
		Mode: RouterModeEdge,
		Replicas: 1,
	}
	return initCommon(&router, []string{"skupper-amqps"}, kube)
}

func initInterior(name string, kube *KubeDetails) *appsv1.Deployment {
	internalCa := ensureCA("skupper-internal-ca", kube)
	router := Router{
		Name: name,
		Mode: RouterModeInterior,
		Replicas: 1,
	}
	dep := initCommon(&router, []string{"skupper-amqps", "skupper-internal"}, kube)
	ensureService("skupper-internal", internalServicePorts(), kube)
	//TODO: handle loadbalancer service where routes are not available
	hosts := ensureRoute("skupper-inter-router", "skupper-internal", "inter-router", kube)
	hosts += "," + ensureRoute("skupper-edge", "skupper-internal", "edge", kube)
	generateSecret(internalCa, "skupper-internal", name, hosts, false, kube)
	return dep
}

func deleteDeployment(name string, kube *KubeDetails) {
	deployments:= kube.Standard.AppsV1().Deployments(kube.Namespace)
	err :=  deployments.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Deployment", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Deployment", name, "doesn't exist")
	} else {
		fmt.Println("Failed to delete deployment ", name, ":", err.Error())
	}
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

func deleteService(name string, kube *KubeDetails) {
	services:= kube.Standard.CoreV1().Services(kube.Namespace)
	err :=  services.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Service", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Service", name, "does not exist")
	} else {
		fmt.Println("Failed to delete service", name, ": ", err.Error())
	}
}

func deleteRoute(name string, kube *KubeDetails) {
	routes := kube.Routes.Routes(kube.Namespace)
	err :=  routes.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Route", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Route", name, "does not exist")
	} else {
		fmt.Println("Failed to delete route", name, ": ", err.Error())
	}
}

func deleteSkupper(kube *KubeDetails) {
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	deleteDeployment("skupper-router", kube)
	deleteDeployment("skupper-proxy-controller", kube)
	deleteSecret("skupper-ca", kube)
	deleteSecret("skupper-amqps", kube)
	deleteSecret("skupper", kube)
	deleteService("skupper-messaging", kube)
	if err == nil && isInterior(current) {
		deleteSecret("skupper-internal-ca", kube)
		deleteSecret("skupper-internal", kube)
		deleteService("skupper-internal", kube)
		deleteRoute("skupper-inter-router", kube)
		deleteRoute("skupper-edge", kube)
	}
}

func connect(secretFile string, connectorName string, kube *KubeDetails) {
	yaml, err := ioutil.ReadFile(secretFile)
        if err != nil {
                panic(err)
        }
	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme,
                scheme.Scheme)
        var secret corev1.Secret
        _, _, err = s.Decode(yaml, nil, &secret)
        if err != nil {
                panic(err)
        }
	secret.ObjectMeta.Name = connectorName
	//determine if local deployment is edge or interior
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		secret.ObjectMeta.SetOwnerReferences([]metav1.OwnerReference{
			metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       current.ObjectMeta.Name,
				UID:        current.ObjectMeta.UID,
			},
		});
		_, err = kube.Standard.CoreV1().Secrets(kube.Namespace).Create(&secret)
		if err == nil {
			//read annotations to get the host and port to connect to
			connector := Connector{
				Name: connectorName,
			}
			if isInterior(current) {
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

func generateConnectSecret(subject string, secretFile string, kube *KubeDetails) {
	//verify that local deployment is interior
	current, err := kube.Standard.AppsV1().Deployments(kube.Namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		if isInterior(current) {
			caSecret, err := kube.Standard.CoreV1().Secrets(kube.Namespace).Get("skupper-internal-ca", metav1.GetOptions{})
			if err == nil {
				//get the host and port for inter-router and edge
				//TODO: handle loadbalancer service where routes are not available
				edgeRoute, err := kube.Routes.Routes(kube.Namespace).Get("skupper-edge", metav1.GetOptions{})
				if err != nil {
					log.Fatal("Could not retrieve edge route: " + err.Error())
				}
				interRouterRoute, err := kube.Routes.Routes(kube.Namespace).Get("skupper-inter-router", metav1.GetOptions{})
				if err != nil {
					log.Fatal("Could not retrieve inter router route: " + err.Error())
				}
				hosts := edgeRoute.Spec.Host + "," + interRouterRoute.Spec.Host
				secret:= certs.GenerateSecret(subject, subject, hosts, caSecret)
				//add annotations for host and port for both edge and inter-router connections
				if secret.ObjectMeta.Annotations == nil {
					secret.ObjectMeta.Annotations = map[string]string{}
				}
				secret.ObjectMeta.Annotations["inter-router-host"] = interRouterRoute.Spec.Host
				secret.ObjectMeta.Annotations["inter-router-port"] = "443"
				secret.ObjectMeta.Annotations["edge-host"] = edgeRoute.Spec.Host
				secret.ObjectMeta.Annotations["edge-port"] = "443"

				//generate yaml and save it to the specified path
				s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
				out, err := os.Create(secretFile)
				if err != nil {
					log.Fatal("Could not write to file " + secretFile + ": " + err.Error())
				}
				err = s.Encode(&secret, out)
				if err != nil {
					log.Fatal("Could not write out generated secret: " + err.Error())
				}
			} else if errors.IsNotFound(err) {
				fmt.Println("Internal CA does not exist: " + err.Error())
			} else {
				fmt.Println("Error retrieving internal CA: " + err.Error())
			}
		} else {
			fmt.Println("Only hub configuration can accepts connections (skupper init --hub true)")
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
                panic(err)
        }

        details.Standard, err = kubernetes.NewForConfig(restconfig)
        if err != nil {
                panic(err)
        }
        details.Routes, err = routev1client.NewForConfig(restconfig)
        if err != nil {
                panic(err.Error())
        }

	if namespace == "" {
		details.Namespace, _, err = kubeconfig.Namespace()
		if err != nil {
			panic(err)
		}
	} else {
		details.Namespace = namespace
	}

	return &details
}

func main() {
	routev1.AddToScheme(scheme.Scheme)
	routev1.AddToSchemeInCoreGroup(scheme.Scheme)

	var silent bool
	var context string
	var namespace string

	var skupperName string
	var isEdge bool
	var enableProxyController bool
	var enableServiceSync bool
	var cmdInit = &cobra.Command{
		Use:   "init",
		Short: "Initialise skupper infrastructure",
		Long: `init will setup a router and other supporting objects to provide a functional skupper installation that can then be connected to other skupper installations`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			kube := initKubeConfig(namespace, context)
			var dep *appsv1.Deployment
			if !isEdge {
				fmt.Println("Initialising skupper")
				dep = initInterior(skupperName, kube)
			} else {
				fmt.Println("Initialising skupper as edge")
				dep = initEdge(skupperName, kube)
			}
			if enableProxyController {
				initProxyController(enableServiceSync, dep, kube)
			}
		},
	}
	cmdInit.Flags().StringVarP(&skupperName, "id", "", "", "Provide a specific identity for the skupper installation")
	cmdInit.Flags().BoolVarP(&isEdge, "edge", "", false, "Configure as an edge")
	cmdInit.Flags().BoolVarP(&enableProxyController, "enable-proxy-controller", "", true, "Setup the proxy controller as well as the router")
	cmdInit.Flags().BoolVarP(&enableServiceSync, "enable-service-sync", "", true, "Configure proxy controller to particiapte in service sync (not relevant if --enable-proxy-controller is false)")

	var cmdDelete = &cobra.Command{
		Use:   "delete",
		Short: "Delete skupper infrastructure",
		Long: `delete will delete any skupper related objects from the namespace`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			deleteSkupper(initKubeConfig(namespace, context))
		},
	}


	var clientIdentity string
	var cmdSecret = &cobra.Command{
		Use:   "secret [path]",
		Short: "Create a secret containing credentials and details needed for another skupper installation to connect to this one",
		Args: requiredArg("path"),
		Run: func(cmd *cobra.Command, args []string) {
			generateConnectSecret(clientIdentity, args[0], initKubeConfig(namespace, context))
		},
	}
	cmdSecret.Flags().StringVarP(&clientIdentity, "client-identity", "i", "skupper", "Provide a specific identity as which connecting skupper installation will be authenticated")

	var connectionName string
	var cmdConnect = &cobra.Command{
		Use:   "connect [secret]",
		Short: "Connect this skupper installation to that which issued the specified secret",
		Args: requiredArg("secret"),
		Run: func(cmd *cobra.Command, args []string) {
			connect(args[0], connectionName, initKubeConfig(namespace, context))
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "name", "", "", "Provide a specific name for the connection (used when removing it with disconnect)")
	cmdConnect.MarkFlagRequired("name")//TODO: make this optional and generate unique name when not specified

	var cmdDisconnect = &cobra.Command{
		Use:   "disconnect [name]",
		Short: "Remove specified connection (NOT YET IMPLEMENTED)",
		Args: requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("disconnect " + args[0] + " NOT YET IMPLEMENTED!")
		},
	}

	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "report status of skupper installation (NOT YET IMPLEMENTED)",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("status NOT YET IMPLEMENTED!")
		},
	}

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdSecret, cmdConnect, cmdDisconnect, cmdStatus)
	rootCmd.PersistentFlags().BoolVarP(&silent, "quiet", "q", false, "reduced output (NOT YET IMPLEMENTED)")
	rootCmd.PersistentFlags().StringVarP(&context, "context", "c", "", "kubeconfig context to use")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "kubernetes namespace to use")
	rootCmd.Execute()
}
