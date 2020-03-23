package smoke

import (
	"fmt"
	"log"
	"os/exec"
	"time"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type SmokeTestRunnerInterface interface {
	Build(public1ConficFile, public2ConficFile, private1ConfigFile, private2ConfigFile string)
	Run()
}

type SmokeTestRunnerBase struct {
	Pub1Cluster  *ClusterContext
	Pub2Cluster  *ClusterContext
	Priv1Cluster *ClusterContext
	Priv2Cluster *ClusterContext
}

func (r *SmokeTestRunnerBase) Build(public1ConficFile, public2ConficFile, private1ConfigFile, private2ConfigFile string) {
	r.Pub1Cluster = BuildClusterContext("public1", public1ConficFile)
	r.Pub2Cluster = BuildClusterContext("public2", public2ConficFile)
	r.Priv1Cluster = BuildClusterContext("private1", private1ConfigFile)
	r.Priv2Cluster = BuildClusterContext("private2", private2ConfigFile)
}

type ClusterContext struct {
	Namespace         string
	ClusterConfigFile string
	Clientset         *kubernetes.Clientset
}

func BuildClusterContext(namespace string, configFile string) *ClusterContext {
	cc := &ClusterContext{}
	cc.Namespace = namespace
	cc.ClusterConfigFile = configFile

	config, err := clientcmd.BuildConfigFromFlags("", configFile)
	if err != nil {
		log.Panic(err.Error())
	}

	cc.Clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}
	return cc
}

func _exec(command string, wait bool) {
	var output []byte
	var err error
	fmt.Println(command)
	cmd := exec.Command("sh", "-c", command)
	if wait {
		output, err = cmd.CombinedOutput()
	} else {
		cmd.Start()
		return
	}
	if err != nil {
		panic(err)
	}
	fmt.Println(string(output))
}

func (cc *ClusterContext) exec(main_command string, sub_command string, wait bool) {
	_exec("KUBECONFIG="+cc.ClusterConfigFile+" "+main_command+" "+cc.Namespace+" "+sub_command, wait)
}

func (cc *ClusterContext) SkupperExec(command string) {
	cc.exec("./skupper -n ", command, true)
}

func (cc *ClusterContext) _kubectl_exec(command string, wait bool) {
	cc.exec("kubectl -n ", command, wait)
}

func (cc *ClusterContext) KubectlExec(command string) {
	cc._kubectl_exec(command, true)
}

func (cc *ClusterContext) KubectlExecAsync(command string) {
	cc._kubectl_exec(command, false)
}

func (cc *ClusterContext) CreateNamespace() {
	NsSpec := &apiv1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cc.Namespace}}
	_, err := cc.Clientset.CoreV1().Namespaces().Create(NsSpec)
	if err != nil {
		log.Panic(err.Error())
	}
}

func (cc *ClusterContext) DeleteNamespace() {
	err := cc.Clientset.CoreV1().Namespaces().Delete(cc.Namespace, &metav1.DeleteOptions{})
	if err != nil {
		log.Panic(err.Error())
	}
}

func (cc *ClusterContext) GetService(name string, timeout_S time.Duration) *apiv1.Service {
	timeout := time.After(timeout_S * time.Second)
	tick := time.Tick(3 * time.Second)
	for {
		select {
		case <-timeout:
			log.Panicln("Timed Out Waiting for service.")
		case <-tick:
			service, err := cc.Clientset.CoreV1().Services(cc.Namespace).Get(name, metav1.GetOptions{})
			if err == nil {
				return service
			} else {
				log.Println("Service not ready yet, current pods state: ")
				cc.KubectlExec("get pods -o wide") //TODO use clientset
			}

		}
	}
}
