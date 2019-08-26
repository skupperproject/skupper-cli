package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	//"path/filepath"
	"regexp"
        "text/tabwriter"
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

	"github.com/skupperproject/skupper-cli/pkg/certs"
)

func usage() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Println("usage: skupper <command> <args>")
	fmt.Println()
	fmt.Println("commands:")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "init\tInitialise skupper infrastructure")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "delete\tDelete skupper infrastructure")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "secret\tGenerate secret for another skupper installation to connect to this one")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "connect\tConnect to another skupper installation")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "disconnect\tRemove connection to another skupper installation")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "describe\tDescribe the current skupper infrastructure")
	w.Flush()
}

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

func mountSecretVolume(name string, router *appsv1.Deployment) {
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
	volumeMounts := router.Spec.Template.Spec.Containers[0].VolumeMounts
	if volumeMounts == nil {
		volumeMounts = []corev1.VolumeMount{}
	}
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: "/etc/qpid-dispatch-certs/" + name + "/",
	})
	router.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
}

func addConnector(connector *Connector, router *appsv1.Deployment) {
	config := findEnvVar(router.Spec.Template.Spec.Containers[0].Env, "QDROUTERD_CONF")
	if config == nil {
		log.Fatal("Could not retrieve router config")
	}
	updated := config.Value + connectorConfig(connector)
	setEnvVar(router, "QDROUTERD_CONF", updated)
	mountSecretVolume(connector.Name, router)
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

func ensureProxyController(namespace string, clientset *kubernetes.Clientset) {
	deployments:= clientset.AppsV1().Deployments(namespace)
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
			image = "quay.io/skupper/icproxy-deployer"
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

		_, err := deployments.Create(dep)
		if err != nil {
			log.Fatal("Failed to create router deployment: " + err.Error())
		}
	} else {
		log.Fatal("Failed to check router deployment: " + err.Error())
	}
}

func ensureRouterDeployment(router *Router, namespace string, volumes []string, clientset *kubernetes.Clientset) *appsv1.Deployment {
	deployments:= clientset.AppsV1().Deployments(namespace)
	existing, err :=  deployments.Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		// Deployment exists, do we need to update it?
		fmt.Println("Router deployment already exists")
		return existing
	} else if errors.IsNotFound(err) {
		routerDeployment := RouterDeployment(router, namespace)
		for _, v := range volumes {
			mountSecretVolume(v, routerDeployment)
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

func ensureCA(name string, namespace string, clientset *kubernetes.Clientset) *corev1.Secret {
	existing, err :=clientset.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
	if err == nil  {
		fmt.Println("CA", name, "already exists")
		return existing
	} else if errors.IsNotFound(err) {
		ca := certs.GenerateCASecret(name, name)
		_, err := clientset.CoreV1().Secrets(namespace).Create(&ca)
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

func ensureService(name string, ports []corev1.ServicePort, namespace string, clientset *kubernetes.Clientset) {
	_, err :=clientset.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
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
		_, err := clientset.CoreV1().Services(namespace).Create(service)
		if err != nil {
			fmt.Println("Failed to create service", name, ": ", err.Error())
		}
	} else {
		fmt.Println("Failed while checking service", name, ": ", err.Error())
	}
}

func ensureRoute(name string, targetService string, targetPort string, namespace string, routeclient *routev1client.RouteV1Client) string {
	_, err := routeclient.Routes(namespace).Get(name, metav1.GetOptions{})
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

		created, err := routeclient.Routes(namespace).Create(route)
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

func generateSecret(caSecret *corev1.Secret, name string, subject string, hosts string, includeConnectJson bool, namespace string, clientset *kubernetes.Clientset) {
	secret := certs.GenerateSecret(name, subject, hosts, caSecret)
	if includeConnectJson {
		secret.Data["connect.json"] = []byte(connectJson())
	}
	_, err := clientset.CoreV1().Secrets(namespace).Create(&secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			//TODO: recreate or just use whats there?
			log.Println("Secret", name, "already exists");
		} else {
			log.Fatal("Could not create secret:", err)
		}
	}
}

func ensureServiceAccount(name string, router *appsv1.Deployment, namespace string, clientset *kubernetes.Clientset) *corev1.ServiceAccount {
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
	actual, err := clientset.CoreV1().ServiceAccounts(namespace).Create(serviceaccount)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Service account", name, "already exists");
		} else {
			log.Fatal("Could not create service account", name, ":", err)
		}

	}
	return actual
}

func ensureViewRole(router *appsv1.Deployment, namespace string, clientset *kubernetes.Clientset) *rbacv1.Role {
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
	actual, err := clientset.RbacV1().Roles(namespace).Create(role)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role", name, "already exists");
		} else {
			log.Fatal("Could not create role", name, ":", err)
		}

	}
	return actual
}

func ensureEditRole(router *appsv1.Deployment, namespace string, clientset *kubernetes.Clientset) *rbacv1.Role {
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
	actual, err := clientset.RbacV1().Roles(namespace).Create(role)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role", name, "already exists");
		} else {
			log.Fatal("Could not create role", name, ":", err)
		}

	}
	return actual
}

func ensureRoleBinding(serviceaccount string, role string, router *appsv1.Deployment, namespace string, clientset *kubernetes.Clientset) {
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
	_, err := clientset.RbacV1().RoleBindings(namespace).Create(rolebinding)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Println("Role binding", name, "already exists");
		} else {
			log.Fatal("Could not create role binding", name, ":", err)
		}

	}
}

func initCommon(router *Router, namespace string, volumes []string, clientset *kubernetes.Clientset) *appsv1.Deployment {
	ca := ensureCA("skupper-ca", namespace, clientset)
	generateSecret(ca, "skupper-amqps", "skupper-messaging", "skupper-messaging,skupper-messaging." + namespace + ".svc.cluster.local", false, namespace, clientset)
	generateSecret(ca, "skupper", "skupper-messaging", "", true, namespace, clientset)
	dep := ensureRouterDeployment(router, namespace, volumes, clientset)
	ensureServiceAccount("skupper", dep, namespace, clientset)
	ensureViewRole(dep, namespace, clientset)
	ensureRoleBinding("skupper", "skupper-view", dep, namespace, clientset)

	ensureService("skupper-messaging", messagingServicePorts(), namespace, clientset)

	return dep
}

func initProxyController(router *appsv1.Deployment, namespace string, clientset *kubernetes.Clientset) {
	ensureServiceAccount("skupper-proxy-controller", router, namespace, clientset)
	ensureEditRole(router, namespace, clientset)
	ensureRoleBinding("skupper-proxy-controller", "skupper-edit", router, namespace, clientset)
	ensureProxyController(namespace, clientset)
}

func initEdge(name string, namespace string, clientset *kubernetes.Clientset) *appsv1.Deployment {
	router := Router{
		Name: name,
		Mode: RouterModeEdge,
		Replicas: 1,
	}
	return initCommon(&router, namespace, []string{"skupper-amqps"}, clientset)
}

func initInterior(name string, namespace string, clientset *kubernetes.Clientset, routeclient *routev1client.RouteV1Client) *appsv1.Deployment {
	internalCa := ensureCA("skupper-internal-ca", namespace, clientset)
	router := Router{
		Name: name,
		Mode: RouterModeInterior,
		Replicas: 1,
	}
	dep := initCommon(&router, namespace, []string{"skupper-amqps", "skupper-internal"}, clientset)
	ensureService("skupper-internal", internalServicePorts(), namespace, clientset)
	//TODO: handle loadbalancer service where routes are not available
	hosts := ensureRoute("skupper-inter-router", "skupper-internal", "inter-router", namespace, routeclient)
	hosts += "," + ensureRoute("skupper-edge", "skupper-internal", "edge", namespace, routeclient)
	generateSecret(internalCa, "skupper-internal", name, hosts, false, namespace, clientset)
	return dep
}

func deleteDeployment(name string, namespace string, clientset *kubernetes.Clientset) {
	deployments:= clientset.AppsV1().Deployments(namespace)
	err :=  deployments.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Deployment", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Deployment", name, "doesn't exist")
	} else {
		fmt.Println("Failed to delete deployment ", name, ":", err.Error())
	}
}

func deleteSecret(name string, namespace string, clientset *kubernetes.Clientset) {
	secrets:= clientset.CoreV1().Secrets(namespace)
	err :=  secrets.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Secret", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Secret", name, "does not exist")
	} else {
		fmt.Println("Failed to delete secret", name, ": ", err.Error())
	}
}

func deleteService(name string, namespace string, clientset *kubernetes.Clientset) {
	services:= clientset.CoreV1().Services(namespace)
	err :=  services.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Service", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Service", name, "does not exist")
	} else {
		fmt.Println("Failed to delete service", name, ": ", err.Error())
	}
}

func deleteRoute(name string, namespace string, routeclient *routev1client.RouteV1Client) {
	routes := routeclient.Routes(namespace)
	err :=  routes.Delete(name, &metav1.DeleteOptions{})
	if err == nil  {
		fmt.Println("Route", name, "deleted")
	} else if errors.IsNotFound(err) {
		fmt.Println("Route", name, "does not exist")
	} else {
		fmt.Println("Failed to delete route", name, ": ", err.Error())
	}
}

func deleteSkupper(namespace string, clientset *kubernetes.Clientset, routeclient *routev1client.RouteV1Client) {
	current, err := clientset.AppsV1().Deployments(namespace).Get("skupper-router", metav1.GetOptions{})
	deleteDeployment("skupper-router", namespace, clientset)
	deleteDeployment("skupper-proxy-controller", namespace, clientset)
	deleteSecret("skupper-ca", namespace, clientset)
	deleteSecret("skupper-amqps", namespace, clientset)
	deleteSecret("skupper", namespace, clientset)
	deleteService("skupper-messaging", namespace, clientset)
	if err == nil && isInterior(current) {
		deleteSecret("skupper-internal-ca", namespace, clientset)
		deleteSecret("skupper-internal", namespace, clientset)
		deleteService("skupper-internal", namespace, clientset)
		deleteRoute("skupper-inter-router", namespace, routeclient)
		deleteRoute("skupper-edge", namespace, routeclient)
	}
}

func connect(secretFile string, connectorName string, namespace string, clientset *kubernetes.Clientset) {
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
	current, err := clientset.AppsV1().Deployments(namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil {
		secret.ObjectMeta.SetOwnerReferences([]metav1.OwnerReference{
			metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       current.ObjectMeta.Name,
				UID:        current.ObjectMeta.UID,
			},
		});
		_, err = clientset.CoreV1().Secrets(namespace).Create(&secret)
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
			_, err = clientset.AppsV1().Deployments(namespace).Update(current)
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

func generateConnectSecret(subject string, secretFile string, namespace string, clientset *kubernetes.Clientset, routeclient *routev1client.RouteV1Client) {
	//verify that local deployment is interior
	current, err := clientset.AppsV1().Deployments(namespace).Get("skupper-router", metav1.GetOptions{})
	if err == nil  {
		if isInterior(current) {
			caSecret, err := clientset.CoreV1().Secrets(namespace).Get("skupper-internal-ca", metav1.GetOptions{})
			if err == nil {
				//get the host and port for inter-router and edge
				//TODO: handle loadbalancer service where routes are not available
				edgeRoute, err := routeclient.Routes(namespace).Get("skupper-edge", metav1.GetOptions{})
				if err != nil {
					log.Fatal("Could not retrieve edge route: " + err.Error())
				}
				interRouterRoute, err := routeclient.Routes(namespace).Get("skupper-inter-router", metav1.GetOptions{})
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

func main() {
	routev1.AddToScheme(scheme.Scheme)
	routev1.AddToSchemeInCoreGroup(scheme.Scheme)

	initCmd := flag.NewFlagSet("init", flag.ExitOnError)
	initName := initCmd.String("name", "", "Name of skupper infrastructure instance")
	initHub := initCmd.Bool("hub", false, "Allow other skupper deployments in other cluster to connect to this deployment")
	initEnableProxyController := initCmd.Bool("enable-proxy-controller", true, "Deploy the proxy-controller as part of the skupper infrastructure")
	deleteCmd := flag.NewFlagSet("delete", flag.ExitOnError)
	describeCmd := flag.NewFlagSet("describe", flag.ExitOnError)
	secretCmd := flag.NewFlagSet("secret", flag.ExitOnError)
	secretPath := secretCmd.String("file", "", "Path to save generated secret")
	secretSubject := secretCmd.String("subject", "", "Subject to use in generated secret")
	connectCmd := flag.NewFlagSet("connect", flag.ExitOnError)
	connectSecret := connectCmd.String("secret", "", "Path to secret generated by skupper deployment targetted by connect")
	connectName := connectCmd.String("name", "", "Name for this connection (used to remove the connection via a disconnect request if desired)")
	disconnectCmd := flag.NewFlagSet("disconnect", flag.ExitOnError)
	disconnectName := disconnectCmd.String("name", "", "Name of connection to remove")

        kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
                clientcmd.NewDefaultClientConfigLoadingRules(),
                &clientcmd.ConfigOverrides{},
        )
        namespace, _, err := kubeconfig.Namespace()
        if err != nil {
                panic(err)
        }

        restconfig, err := kubeconfig.ClientConfig()
        if err != nil {
                panic(err)
        }

        clientset, err := kubernetes.NewForConfig(restconfig)
        if err != nil {
                panic(err)
        }
        routeclient, err := routev1client.NewForConfig(restconfig)
        if err != nil {
                panic(err.Error())
        }


	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		initCmd.Parse(os.Args[2:])
		var dep *appsv1.Deployment
		if *initHub {
			fmt.Println("Initialising skupper hub")
			dep = initInterior(*initName, namespace, clientset, routeclient)
		} else {
			fmt.Println("Initialising skupper edge")
			dep = initEdge(*initName, namespace, clientset)
		}
		if *initEnableProxyController {
			initProxyController(dep, namespace, clientset)
		}
	case "delete":
		deleteCmd.Parse(os.Args[2:])
		deleteSkupper(namespace, clientset, routeclient)
	case "describe":
		describeCmd.Parse(os.Args[2:])
		fmt.Println("describe not yet implemented")
	case "disconnect":
		disconnectCmd.Parse(os.Args[2:])
		fmt.Println("disconnect not yet implemented", *disconnectName)
	case "connect":
		connectCmd.Parse(os.Args[2:])
		connect(*connectSecret, *connectName, namespace, clientset)
	case "secret":
		secretCmd.Parse(os.Args[2:])
		generateConnectSecret(*secretSubject, *secretPath, namespace, clientset, routeclient)
	default:
		usage();
		os.Exit(1)
	}
}
