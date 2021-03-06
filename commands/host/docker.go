/*
 *  Copyright 2016 Adobe Systems Incorporated. All rights reserved.
 *  This file is licensed to you under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License. You may obtain a copy
 *  of the License at http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software distributed under
 *  the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR REPRESENTATIONS
 *  OF ANY KIND, either express or implied. See the License for the specific language
 *  governing permissions and limitations under the License.
 */
package host

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/adobe-platform/porter/aws_session"
	"github.com/adobe-platform/porter/conf"
	"github.com/adobe-platform/porter/constants"
	dockerutil "github.com/adobe-platform/porter/docker/util"
	"github.com/adobe-platform/porter/logger"
	"github.com/adobe-platform/porter/secrets"
	"github.com/adobe-platform/porter/util"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/inconshreveable/log15"
	"github.com/phylake/go-cli"
)

// This implementation is tightly coupled with HAProxyCmd and how these commands
// are called together
type DockerCmd struct{}

func (recv *DockerCmd) Name() string {
	return "docker"
}

func (recv *DockerCmd) ShortHelp() string {
	return "Docker container orchestration"
}

func (recv *DockerCmd) LongHelp() string {
	return `NAME
    docker -- Docker container orchestration

SYNOPSIS
    docker --start -e <environment> -r <region>
    docker --clean
    docker --ip

DESCRIPTION
    docker manipulates docker containers.

    This command expects to receive a porter config on STDIN.

OPTIONS
    --start
        Start and configure all primary and secondary containers found in the
        config. Also, print to STDOUT the primary container's host port that is
        to be mapped in HAProxy

    -e  Environment from .porter/config

    -r  AWS region

    --clean
        Cleanup containers not found in the config. This command removes
        old containers and images with the equivalent of
            docker stop <container id>
            docker rm <container id>
            docker rmi <image id>
        This is important in terms of resource utilization that the containers
        are no longer running and the disk space the images occupy is released
        for long-running services that typically only do a hotswap

    --ip
        print the docker interface's IPv4 address to STDOUT`
}

func (recv *DockerCmd) SubCommands() []cli.Command {
	return nil
}

func (recv *DockerCmd) Execute(args []string) bool {
	if len(args) > 0 {
		switch args[0] {
		case "--start":
			if len(args) == 1 {
				return false
			}

			var environment, region string
			flagSet := flag.NewFlagSet("", flag.ExitOnError)
			flagSet.StringVar(&environment, "e", "", "")
			flagSet.StringVar(&region, "r", "", "")
			flagSet.Usage = func() {
				fmt.Println(recv.LongHelp())
			}
			flagSet.Parse(args[1:])

			startContainers(environment, region)
		case "--clean":
			if len(args) == 1 {
				return false
			}

			var environment, region string
			flagSet := flag.NewFlagSet("", flag.ExitOnError)
			flagSet.StringVar(&environment, "e", "", "")
			flagSet.StringVar(&region, "r", "", "")
			flagSet.Usage = func() {
				fmt.Println(recv.LongHelp())
			}
			flagSet.Parse(args[1:])

			cleanContainers(environment, region)
		case "--ip":
			printIPv4()
		default:
			return false
		}
		return true
	}

	return false
}

func startContainers(environmentStr, regionStr string) {
	var (
		err                  error
		success              bool
		containerToDstEnvKey map[string]string
		stdoutBuf            bytes.Buffer
		haproxyStdin         HAPStdin
	)

	log := logger.Host("cmd", "docker")

	config, getStdinConfigSucces := conf.GetStdinConfig(log)
	if !getStdinConfigSucces {
		os.Exit(1)
	}

	environment, err := config.GetEnvironment(environmentStr)
	if err != nil {
		log.Crit("GetEnvironment", "Error", err)
		os.Exit(1)
	}

	region, err := environment.GetRegion(regionStr)
	if err != nil {
		log.Crit("GetRegion", "Error", err)
		os.Exit(1)
	}

	log.Info("starting docker containers")

	dockerIPv4 := dockerIfaceIPv4(log)

	if !prepareNetwork(log) {
		os.Exit(1)
	}

	containerToDstEnvKey, success = getContainerToDstEnvS3Key(log, region.Name)
	if !success {
		os.Exit(1)
	}

	for _, container := range region.Containers {

		runArgs := []string{
			"run",

			// daemonize
			"-d",

			// log driver with defaults since facility override doesn't work
			"--log-driver=syslog",

			// try to keep the container alive
			// CIS Docker Benchmark 1.11.0 5.14
			"--restart=on-failure:5",

			// set ulimit for container
			// TODO calculate this
			"--ulimit", "nofile=200000",

			"--net", "porter",

			// Read in additional variables written during bootstrap
			"--env-file", constants.EnvFile,

			// who and where am i?
			"-e", "PORTER_ENVIRONMENT=" + environment.Name,
			"-e", "AWS_REGION=" + region.Name,

			// rsyslog
			"-e", "RSYSLOG_TCP_ADDR=" + dockerIPv4,
			"-e", "RSYSLOG_TCP_PORT=514",
			"-e", "RSYSLOG_UDP_ADDR=" + dockerIPv4,
			"-e", "RSYSLOG_UDP_PORT=514",

			// porterd
			"-e", "PORTERD_TCP_ADDR=" + dockerIPv4,
			"-e", "PORTERD_TCP_PORT=" + constants.PorterDaemonBindPort,
		}

		if container.Topology == conf.Topology_Inet {
			// publish to an ephemeral port
			runArgs = append(runArgs, "-P")
		}

		if container.ReadOnly == nil || *container.ReadOnly == true {
			// CIS Docker Benchmark 1.11.0 5.12
			runArgs = append(runArgs, "--read-only")
		}

		// TODO revisit --cap-drop=ALL with override https://docs.docker.com/engine/reference/run/#runtime-privilege-and-linux-capabilities
		if container.Uid == nil {
			runArgs = append(runArgs, "-u", constants.ContainerUserUid)
		} else {
			runArgs = append(runArgs, "-u", strconv.Itoa(*container.Uid))
		}

		runArgs = append(runArgs, getSecretEnvVars(log, region.Name, container, containerToDstEnvKey)...)

		runArgs = append(runArgs, container.Name)

		if container.Topology == conf.Topology_Inet {

			cmd := exec.Command("docker", runArgs...)
			cmd.Stdout = &stdoutBuf
			err = cmd.Run()
			if err != nil {
				log.Crit("docker run", "Error", err)
				os.Exit(1)
			}

			containerId := strings.TrimSpace(stdoutBuf.String())
			if containerId == "" {
				log.Crit("missing container id")
				os.Exit(1)
			}
			stdoutBuf.Reset()

			hostPort, hostPortsuccess := getInetHostPort(log, container.InetPort, containerId)
			if !hostPortsuccess {
				os.Exit(1)
			}

			hapContainer := HAPContainer{
				Id:                containerId,
				HealthCheckMethod: container.HealthCheck.Method,
				HealthCheckPath:   container.HealthCheck.Path,
				HostPort:          hostPort,
			}

			haproxyStdin.Containers = append(haproxyStdin.Containers, hapContainer)
		} else {

			err = exec.Command("docker", runArgs...).Run()
			if err != nil {
				log.Crit("docker run", "Error", err)
				os.Exit(1)
			}
		}
	}

	stdoutBytes, err := json.Marshal(haproxyStdin)
	if err != nil {
		log.Error("json.Marshal", "Error", err)
		os.Exit(1)
	}

	_, err = os.Stdout.Write(stdoutBytes)
	if err != nil {
		log.Error("os.Stdout.Write", "Error", err)
		os.Exit(1)
	}
}

func prepareNetwork(log log15.Logger) (success bool) {
	var stdoutBuf bytes.Buffer

	cmd := exec.Command("docker", "network", "ls")
	cmd.Stdout = &stdoutBuf
	err := cmd.Run()
	if err != nil {
		log.Crit("docker network ls", "Error", err)
		return
	}

	networks, err := dockerutil.NetworkNameToId(&stdoutBuf)
	if err != nil {
		log.Error("NetworkNameToId", "Error", err)
		return
	}

	foundNetwork := false
	for networkName := range networks {
		if networkName == "porter" {
			foundNetwork = true
			break
		}
	}

	if !foundNetwork {
		err = exec.Command("docker", "network", "create", "porter").Run()
		if err != nil {
			log.Crit("docker network create porter", "Error", err)
			return
		}
	}

	success = true
	return
}

func getInetHostPort(log log15.Logger, inetContainerPort int, containerId string) (hostPort uint16, success bool) {

	var stdoutBuf bytes.Buffer

	//
	// Get port mappings for container id
	//
	inspectFilter := "{{ range $containerPort, $host := .NetworkSettings.Ports }}{{ println $containerPort (index $host 0).HostPort }}{{ end }}"
	cmd := exec.Command("docker", "inspect", "-f", inspectFilter, containerId)
	cmd.Stdout = &stdoutBuf
	err := cmd.Run()
	if err != nil {
		log.Crit("docker inspect", "Error", err)
		return
	}

	// portMappings should look like this []string{"1234/tcp 56789"}
	portMappings := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	stdoutBuf.Reset()

	if len(portMappings) == 0 {
		log.Crit("No port mappings found. Does the Dockerfile EXPOSE any ports?")
		return
	}

	if inetContainerPort == 0 && len(portMappings) > 1 {
		log.Crit("There are multiple EXPOSEd ports and no designated internet port")
		return
	}

	for _, portMapping := range portMappings {
		mappingParts := strings.Split(portMapping, " ")
		containerPortParts := strings.Split(mappingParts[0], "/")

		containerPort := containerPortParts[0]
		containerProtocol := containerPortParts[1]
		hostPortStr := mappingParts[1]

		log.Info("port mapping", "HostPort", hostPortStr, "ContainerPort", containerPort, "ContainerProtocol", containerProtocol)

		containerPortInt, err := strconv.Atoi(containerPort)
		if err != nil {
			log.Crit("Atoi", "Error", err, "PortMapping", portMapping)
			return
		}

		// either there's a single container and no configured inet_port (validated above)
		// or we wait for a match on the configured inet_port
		if inetContainerPort == 0 || inetContainerPort == containerPortInt {
			if containerProtocol != "tcp" {
				log.Crit("cannot route internet traffic to a protocol other than TCP", "PortMapping", portMapping)
				return
			}

			log.Info("inet container", "HostPort", containerPort)

			hostPortInt, err := strconv.Atoi(hostPortStr)
			if err != nil {
				log.Error("strconv.Atoi", "Error", err)
				return
			}

			hostPort = uint16(hostPortInt)
			success = true
			return
		}
	}

	return
}

func cleanContainers(environmentStr, regionStr string) {
	var err error

	log := logger.Host("cmd", "docker")

	config, getStdinConfigSucces := conf.GetStdinConfig(log)
	if !getStdinConfigSucces {
		os.Exit(1)
	}

	environment, err := config.GetEnvironment(environmentStr)
	if err != nil {
		log.Error("GetEnvironment", "Error", err)
		os.Exit(1)
	}

	region, err := environment.GetRegion(regionStr)
	if err != nil {
		log.Error("GetRegion", "Error", err)
		os.Exit(1)
	}

	log.Info("cleaning up docker containers")

	activeContainers := make(map[string]interface{})

	for _, container := range region.Containers {
		activeContainers[container.Name] = nil
	}

	psOutput, err := exec.Command("docker", "ps", "-q").Output()
	if err != nil {
		log.Crit("docker ps", "Error", err)
		os.Exit(1)
	}

	anyError := false

	containerIds := strings.Split(strings.TrimSpace(string(psOutput)), "\n")
	for _, containerId := range containerIds {

		inspectOutput, err := exec.Command("docker", "inspect", "-f", "{{ .Config.Image }}", containerId).Output()
		if err != nil {
			anyError = true
			log.Error("docker inspect", "ContainerId", containerId, "Error", err)
			continue
		}

		imageName := strings.TrimSpace(string(inspectOutput))
		imageNameParts := strings.Split(imageName, "-")

		// not an image name that we manage
		if len(imageNameParts) != 3 {
			continue
		}

		if _, exists := activeContainers[imageName]; exists {
			continue
		}

		drainConnections(log, containerId)

		log.Info("docker stop " + containerId)
		err = exec.Command("docker", "stop", containerId).Run()
		if err != nil {
			anyError = true
			log.Error("docker stop", "ContainerId", containerId, "Error", err)
			continue
		}

		log.Info("docker rm " + containerId)
		err = exec.Command("docker", "rm", containerId).Run()
		if err != nil {
			anyError = true
			log.Error("docker rm", "ContainerId", containerId, "Error", err)
			continue
		}

		log.Info("docker rmi " + imageName)
		err = exec.Command("docker", "rmi", imageName).Run()
		if err != nil {
			anyError = true
			log.Error("docker rmi", "ImageName", imageName, "Error", err)
			continue
		}
	}

	if anyError {
		log.Error("cleanup encountered errors")
	} else {
		log.Info("cleanup succeeded")
	}
}

func drainConnections(log log15.Logger, containerId string) (success bool) {
	var (
		err       error
		stdoutBuf bytes.Buffer
	)
	log = log.New("ContainerId", containerId)

	inspectFilter := "{{ range $containerPort, $host := .NetworkSettings.Ports }}{{ println $containerPort (index $host 0).HostPort }}{{ end }}"
	inspectOut, err := exec.Command("docker", "inspect", "-f", inspectFilter, containerId).Output()
	if err != nil {
		log.Crit("docker inspect", "Error", err)
		return
	}

	// portMappings should look like this []string{"1234/tcp 56789"}
	portMappings := strings.Split(strings.TrimSpace(string(inspectOut)), "\n")
	stdoutBuf.Reset()

	if len(portMappings) == 0 {
		success = true
		return
	}

	for _, portMapping := range portMappings {
		mappingParts := strings.Split(portMapping, " ")
		hostPort := mappingParts[1]

		log = log.New("HostPort", hostPort)
		log.Info("Draining connections on container")

		for {
			// Don't handle err because once the container is stopped by haproxy
			// meaning the hostPort is no longer connected then lsof exits 1
			//
			// Since the error will always happen there's no point handling it.
			lsofOut, _ := exec.Command("lsof", "-i@localhost:"+hostPort).Output()

			connections := strings.Split(strings.TrimSpace(string(lsofOut)), "\n")

			// an artifact of splitting an empty string is a slice of length 1
			connectionCount := len(connections) - 1
			log.Info("Connection drain", "Connections", connectionCount)

			// use <= to cover Split behavior changing in the future
			if connectionCount <= 0 {
				break
			}

			time.Sleep(1 * time.Second)
		}
	}

	success = true
	return
}

func printIPv4() {
	log := logger.Host("cmd", "docker")
	fmt.Fprint(os.Stdout, dockerIfaceIPv4(log))
}

func dockerIfaceIPv4(log log15.Logger) string {
	iface, err := net.InterfaceByName("docker0")
	if err != nil {
		log.Crit("InterfaceByName docker0", "Error", err)
		os.Exit(1)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		log.Crit("iface.Addrs()", "Error", err)
		os.Exit(1)
	}
	if len(addrs) == 0 {
		log.Crit("No IP for iface docker0")
		os.Exit(1)
	}

	for _, addr := range addrs {

		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			log.Error("ParseCIDR docker0", "Error", err)
			continue
		}

		if ip.To4() != nil {
			return ip.String()
		}
	}

	log.Crit("Couldn't find IPv4 address for iface docker0")
	os.Exit(1)
	return ""
}

func getSecretEnvVars(log log15.Logger, region string, container *conf.Container,
	containerToDstEnvKey map[string]string) (runArgs []string) {

	runArgs = make([]string, 0)

	if container.DstEnvFile == nil {
		return
	}

	containerSecretsEncPayload := getSecretsPayload(log, region, container, containerToDstEnvKey)
	containerSecretsKey := getSecretsKey(log, region)

	containerSecrets, err := secrets.Decrypt(containerSecretsEncPayload, containerSecretsKey)
	if err != nil {
		log.Crit("secrets.Decrypt", "Error", err)
		os.Exit(1)
	}

	kvps := strings.Split(string(containerSecrets), "\n")

	for _, kvp := range kvps {
		runArgs = append(runArgs, "-e", kvp)
	}

	return
}

func getSecretsPayload(log log15.Logger, region string, container *conf.Container,
	containerToDstEnvKey map[string]string) []byte {

	s3Client := s3.New(aws_session.Get(region))

	var (
		dstEnvKey string
		exists    bool
	)

	dstEnvKey, exists = containerToDstEnvKey[container.Name]
	if !exists {
		log.Crit("DstEnvFile config exists but no S3 key was found",
			"Container", container.Name,
		)
		os.Exit(1)
	}

	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(container.DstEnvFile.S3Bucket),
		Key:    aws.String(dstEnvKey),
	}

	getObjectOutput, err := s3Client.GetObject(getObjectInput)
	if err != nil {
		log.Crit("GetObject",
			"Error", err,
			"Container", container.Name,
			"DstEnvFile.S3Bucket", container.DstEnvFile.S3Bucket,
			"DstEnvFile.KMSARN", container.DstEnvFile.KMSARN,
		)
		os.Exit(1)
	}
	defer getObjectOutput.Body.Close()

	getObjectBytes, err := ioutil.ReadAll(getObjectOutput.Body)
	if err != nil {
		log.Crit("ioutil.ReadAll",
			"Error", err,
			"Container", container.Name,
			"DstEnvFile.S3Bucket", container.DstEnvFile.S3Bucket,
			"DstEnvFile.KMSARN", container.DstEnvFile.KMSARN,
		)
		os.Exit(1)
	}

	return getObjectBytes
}

func getSecretsKey(log log15.Logger, region string) []byte {

	var (
		describeStacksOutput *cloudformation.DescribeStacksOutput
		err                  error
	)

	cfnClient := cloudformation.New(aws_session.Get(region))

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(os.Getenv("AWS_STACKID")),
	}

	retryMsg := func(i int) { log.Warn("DescribeStacks retrying", "Count", i) }
	if !util.SuccessRetryer(9, retryMsg, func() bool {
		describeStacksOutput, err = cfnClient.DescribeStacks(describeStacksInput)
		if err != nil {
			log.Error("DescribeStacks", "Error", err)
			return false
		}
		if len(describeStacksOutput.Stacks) == 0 {
			log.Error("len(describeStacksOutput.Stacks == 0)")
			return false
		}
		return true
	}) {
		log.Crit("Failed to DescribeStacks")
		os.Exit(1)
	}

	if len(describeStacksOutput.Stacks) != 1 {
		log.Crit("len(describeStacksOutput.Stacks != 1)")
		os.Exit(1)
	}

	stack := describeStacksOutput.Stacks[0]
	for _, param := range stack.Parameters {
		if *param.ParameterKey == constants.ParameterSecretsKey {

			symmetricKey, err := hex.DecodeString(*param.ParameterValue)
			if err != nil {
				log.Crit("hex.DecodeString", "Error", err)
				os.Exit(1)
			}

			return symmetricKey
		}
	}

	log.Crit("missing parameter key " + constants.ParameterSecretsKey)
	os.Exit(1)
	return nil
}

func getContainerToDstEnvS3Key(log log15.Logger, region string) (containerToDstEnvKey map[string]string, success bool) {
	var (
		describeStackResourceOutput *cloudformation.DescribeStackResourceOutput
		err                         error
	)

	log = log.New("StackId", os.Getenv("AWS_STACKID"))

	cfnClient := cloudformation.New(aws_session.Get(region))

	containerToDstEnvKey = make(map[string]string)

	tagsUrl := fmt.Sprintf("http://localhost:%s/aws/ec2/tags", constants.PorterDaemonBindPort)
	tagsResp, err := http.Get(tagsUrl)
	if err != nil {
		log.Error("GET "+tagsUrl, "Error", err)
		return
	}
	defer tagsResp.Body.Close()

	tagsMap := make(map[string]string)

	err = json.NewDecoder(tagsResp.Body).Decode(&tagsMap)
	if err != nil {
		log.Error("Couldn't deserialize response",
			"URL", tagsUrl,
			"Error", err,
		)
	}

	var waitHandleLogicalId string
	for key, value := range tagsMap {
		if key == constants.PorterWaitConditionHandleLogicalIdTag {
			waitHandleLogicalId = value
			break
		}
	}

	if waitHandleLogicalId == "" {
		log.Error("Couldn't retrieve WaitHandle Logical Id")
		return
	}

	describeStackResourceInput := &cloudformation.DescribeStackResourceInput{
		LogicalResourceId: aws.String(waitHandleLogicalId),
		StackName:         aws.String(os.Getenv("AWS_STACKID")),
	}

	retryMsg := func(i int) { log.Warn("DescribeStackResource retrying", "Count", i) }
	if !util.SuccessRetryer(9, retryMsg, func() bool {
		describeStackResourceOutput, err = cfnClient.DescribeStackResource(describeStackResourceInput)
		if err != nil {
			log.Error("DescribeStackResource", "Error", err)
			return false
		}
		if describeStackResourceOutput.StackResourceDetail == nil {
			log.Error("describeStackResourceOutput.StackResourceDetail == nil")
			return false
		}
		if describeStackResourceOutput.StackResourceDetail.Metadata == nil {
			log.Error("describeStackResourceOutput.StackResourceDetail.Metadata == nil")
			return false
		}
		return true
	}) {
		return
	}

	metadataStr := *describeStackResourceOutput.StackResourceDetail.Metadata

	metadata := make(map[string]interface{})
	err = json.NewDecoder(strings.NewReader(metadataStr)).Decode(&metadata)
	if err != nil {
		log.Error("json.Marshal",
			"Error", err,
			"metadataStr", metadataStr)
		return
	}

	if msi, ok := metadata[constants.MetadataAsEnvFiles].(map[string]interface{}); ok {

		for key, value := range msi {
			if strVal, ok := value.(string); ok {
				containerToDstEnvKey[key] = strVal
			} else {
				log.Error("Type assertion failed")
				return
			}
		}
	} else {
		log.Error("Missing " + constants.MetadataAsEnvFiles + " on " + waitHandleLogicalId + ".Metadata")
		return
	}

	success = true
	return
}
