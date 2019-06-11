package localdev

import (
	"bytes"
	"context"
	"fmt"
	"github.com/distributed-containers-inc/sanic/util"
	"golang.org/x/sync/errgroup"
	"os"
	"os/exec"
	"os/user"
	"sigs.k8s.io/kind/pkg/cluster"
	kindconfig "sigs.k8s.io/kind/pkg/cluster/config"
	"sigs.k8s.io/kind/pkg/cluster/config/encoding"
	"sigs.k8s.io/kind/pkg/cluster/create"
	kindnode "sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/container/cri"
	"strings"
	"time"
)

var kindContext = cluster.NewContext("sanic")

func clusterNodes() ([]kindnode.Node, error) {
	return kindnode.List("label=io.k8s.sigs.kind.cluster=sanic")
}

func clusterMasterNodes() ([]kindnode.Node, error) {
	return kindnode.List("label=io.k8s.sigs.kind.cluster=sanic", "label=io.k8s.sigs.kind.role=control-plane")
}

func (provisioner *ProvisionerLocalDev) checkClusterReady() error {
	cmd := exec.Command(
		"kubectl",
		"--kubeconfig="+provisioner.KubeConfigLocation(),
		"get",
		"nodes",
		"-o",
		"jsonpath={.items..status.conditions[-1:].lastTransitionTime}\t{.items..status.conditions[-1:].status}",
	)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("could not check if the cluster was running: %s %s", err.Error(), stderr.String())
	}
	//output is, e.g., "2019-06-02T01:04:02Z 2019-06-02T01:04:18Z 2019-06-02T01:04:17Z 2019-06-02T01:04:17Z\tTrue True True True"
	split := strings.Split(stdout.String(), "\t")
	if len(split) != 2 {
		return fmt.Errorf("got invalid kubernetes output while checking if cluster was running: \"%s\"", stdout.String())
	}
	nodeTimes := strings.Split(split[0], " ")
	nodesReady := strings.Split(split[1], " ")

	if len(nodeTimes) != 4 {
		return fmt.Errorf("some nodes were not running, we were expecting 4 (3 workers + one master node)")
	}

	allNodesReady := true
	for _, nodeReady := range nodesReady {
		nodeReady = strings.TrimSpace(nodeReady)
		if nodeReady != "True" {
			allNodesReady = false
		}
	}
	if allNodesReady {
		return nil
	}

	statusChangeRecent := false

	for _, timeString := range nodeTimes {
		timeString = strings.TrimSpace(timeString)
		//TODO will kubernetes ever give iso 8601 formatted date which is not this specific format?
		//Mon Jan 2 15:04:05 MST 2006
		statusTime, err := time.Parse("2006-01-02T15:04:05Z", timeString)
		if err != nil {
			return fmt.Errorf("got invalid kubernetes output while checking how long cluster has been running: %s", timeString)
		}
		if statusTime.Add(time.Minute).After(time.Now()) {
			//status has changed less than a minute ago
			statusChangeRecent = true
		}
	}

	if statusChangeRecent {
		for {
			fmt.Print("do you want to redeploy recently started kubernetes cluster? [y/N]: ")
			var resp string
			fmt.Scanln(&resp)
			switch resp {
			case "y", "Y":
				return fmt.Errorf("some nodes weren't ready, and you chose to redeploy")
			case "n", "N", "":
				return nil
			default:
				fmt.Printf("Did not understand response: %s, expected y/n\n", resp)
			}
		}
	}
	return fmt.Errorf("cluster is not ready, and has not been for over a minute")
}

func (provisioner *ProvisionerLocalDev) checkCluster() error {
	nodes, err := clusterNodes()
	if err != nil {
		return err
	}

	requiredContainersRunning := map[string]*kindnode.Node{
		"sanic-worker":        nil,
		"sanic-worker2":       nil,
		"sanic-worker3":       nil,
		"sanic-control-plane": nil,
	}

	for _, node := range nodes {
		if _, ok := requiredContainersRunning[node.Name()]; ok {
			requiredContainersRunning[node.Name()] = &node
		}
	}

	if len(nodes) == 0 {
		return fmt.Errorf("no nodes were running, cluster has to be provisioned once per docker engine restart")
	}

	if len(nodes) != len(requiredContainersRunning) {
		return fmt.Errorf("some nodes have been removed/crashed. only %d/%d were running",
			len(nodes), len(requiredContainersRunning))
	}
	for _, node := range requiredContainersRunning {
		if node == nil {
			return fmt.Errorf("some nodes were not running while others were, try deleting your cluster containers with docker rm")
		}
	}

	return provisioner.checkClusterReady()
}

func deleteClusterContainers() error {
	nodes, err := clusterNodes()
	if err != nil {
		return err
	}
	eg := errgroup.Group{}
	for _, node := range nodes {
		name := node.Name()
		eg.Go(func() error {
			cmd := exec.Command("docker", "rm", "-f", name)
			return cmd.Run()
		})
	}
	return eg.Wait()
}

const nodeRegistryConfigPatch = `
grep -q '[REGISTRY]' /etc/containerd/config.toml || \
{ sed -i -e '/\[plugins\.cri\.registry\.mirrors\]/a\' \
  -e '        [plugins.cri.registry.mirrors."[REGISTRY]"]\' \
  -e '          endpoint = ["http://[REGISTRY]"]' \
  /etc/containerd/config.toml;
  systemctl restart containerd;
}
`

//patchRegistryContainers makes the internal docker registry trusted by the nodes, to allow local pushes there
//the config patch checks if the registry has already been patched,
//  if it hasn't been patched, it inserts two new lines in /etc/containerd/config.toml to allow insecure pulls via HTTP
//  from it, and then restarts containerd for the configuration change to take effect
func (provisioner *ProvisionerLocalDev) patchRegistryContainers(ctx context.Context) error {
	nodes, err := clusterNodes()
	if err != nil {
		return err
	}

	registry, err := provisioner.Registry()
	if err != nil {
		return err
	}

	var funcs []func(context.Context) error
	for _, node := range nodes {
		containerIdentifier := node.Name()
		funcs = append(funcs, func(ctx context.Context) error {
			cmd := exec.Command(
				"docker", "exec", containerIdentifier,
				"bash", "-c",
				strings.ReplaceAll(nodeRegistryConfigPatch, `[REGISTRY]`, registry),
			)
			cmd.Start()
			return util.WaitCmdContextually(ctx, cmd)
		})
	}
	return util.RunContextuallyInParallel(ctx, funcs...)
}

func (provisioner *ProvisionerLocalDev) startCluster() error {
	usr, err := user.Current()
	if err != nil {
		return fmt.Errorf("could not find your home directory: %s", err.Error())
	}

	cfg := kindconfig.Cluster{}
	encoding.Scheme.Default(&cfg)
	nodeMounts := []cri.Mount{
		{
			ContainerPath: "/hosthome",
			HostPath:      usr.HomeDir,
			Readonly:      true,
		},
	}
	if _, err := os.Stat("/mnt"); err == nil {
		nodeMounts = append(nodeMounts, cri.Mount{
			ContainerPath: "/mnt",
			HostPath: "/mnt",
			Readonly: true,
		})
	}

	cfg.Nodes = []kindconfig.Node{
		{
			Role:        kindconfig.ControlPlaneRole,
			ExtraMounts: nodeMounts,
		},
		{
			Role:        kindconfig.WorkerRole,
			ExtraMounts: nodeMounts,
		},
		{
			Role:        kindconfig.WorkerRole,
			ExtraMounts: nodeMounts,
		},
		{
			Role:        kindconfig.WorkerRole,
			ExtraMounts: nodeMounts,
		},
	}

	//TODO HACK: kind does not always work if the containers are not manually removed first
	if err := deleteClusterContainers(); err != nil {
		//noinspection ALL
		return fmt.Errorf("could not delete existing containers to run cluster setup: %s. Is the docker engine running?", err.Error())
	}

	return kindContext.Create(&cfg, create.Retain(false))
}
