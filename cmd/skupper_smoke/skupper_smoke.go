package main

import (
	"flag"
	"path/filepath"

	"k8s.io/client-go/util/homedir"

	"github.com/skupperproject/skupper-cli/pkg/smoke"
	"github.com/skupperproject/skupper-cli/pkg/smoke/tcp_echo"
)

func main() {
	testRunners := []smoke.SmokeTestRunnerInterface{&tcp_echo.SmokeTestRunner{}}

	defaultKubeConfig := filepath.Join(homedir.HomeDir(), ".kube", "config")

	pub1Kubeconfig := flag.String("pub1kubeconfig", defaultKubeConfig, "(optional) absolute path to the kubeconfig file")
	pub2Kubeconfig := flag.String("pub2kubeconfig", defaultKubeConfig, "(optional) absolute path to the kubeconfig file")
	priv1Kubeconfig := flag.String("priv1kubeconfig", defaultKubeConfig, "(optional) absolute path to the kubeconfig file")
	priv2Kubeconfig := flag.String("priv2kubeconfig", defaultKubeConfig, "(optional) absolute path to the kubeconfig file")

	flag.Parse()

	for _, testRunner := range testRunners {
		testRunner.Build(*pub1Kubeconfig, *pub2Kubeconfig, *priv1Kubeconfig, *priv2Kubeconfig)
		testRunner.Run()
	}
}
