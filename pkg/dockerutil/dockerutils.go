package dockerutil

import (
	"bufio"
	"bytes"
	"fmt"
	exec2 "github.com/drud/ddev/pkg/exec"
	"github.com/drud/ddev/pkg/nodeps"
	"github.com/drud/ddev/pkg/version"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"net/url"

	"github.com/Masterminds/semver"
	"github.com/drud/ddev/pkg/output"
	"github.com/fsouza/go-dockerclient"
)

// NetName provides the default network name for ddev.
const NetName = "ddev_default"

// EnsureNetwork will ensure the docker network for ddev is created.
func EnsureNetwork(client *docker.Client, name string) error {
	if !NetExists(client, name) {
		netOptions := docker.CreateNetworkOptions{
			Name:     name,
			Driver:   "bridge",
			Internal: false,
		}
		_, err := client.CreateNetwork(netOptions)
		if err != nil {
			return err
		}
		output.UserOut.Println("Network", name, "created")

	}
	return nil
}

// EnsureDdevNetwork just creates or ensures the ddev_default network exists or
// exits with fatal.
func EnsureDdevNetwork() {
	// ensure we have docker network
	client := GetDockerClient()
	err := EnsureNetwork(client, NetName)
	if err != nil {
		log.Fatalf("Failed to ensure docker network %s: %v", NetName, err)
	}
}

// GetDockerClient returns a docker client for a docker-machine.
func GetDockerClient() *docker.Client {
	client, err := docker.NewClientFromEnv()
	if err != nil {
		output.UserOut.Warnf("could not get docker client. is docker running? error: %v", err)
		// Use os.Exit instead of util.Failed() to avoid import cycle with util.
		os.Exit(100)
	}

	return client
}

// FindContainerByName takes a container name and returns the container
func FindContainerByName(name string) (*docker.APIContainers, error) {
	containers, err := GetDockerContainers(true)
	if err != nil {
		return nil, err
	}
	// First, ensure a site name is set and matches the current application.
	for _, container := range containers {
		if len(container.Names) > 0 && container.Names[0] == "/"+name {
			return &container, nil
		}
	}
	return nil, nil
}

// FindContainerByLabels takes a map of label names and values and returns any docker containers which match all labels.
func FindContainerByLabels(labels map[string]string) (*docker.APIContainers, error) {
	containers, err := FindContainersByLabels(labels)
	if err != nil {
		return nil, err
	}
	if len(containers) > 0 {
		return &containers[0], nil
	}
	return nil, nil
}

// GetDockerContainers returns a slice of all docker containers on the host system.
func GetDockerContainers(allContainers bool) ([]docker.APIContainers, error) {
	client := GetDockerClient()
	containers, err := client.ListContainers(docker.ListContainersOptions{All: allContainers})
	return containers, err
}

// FindContainersByLabels takes a map of label names and values and returns any docker containers which match all labels.
func FindContainersByLabels(labels map[string]string) ([]docker.APIContainers, error) {
	var returnError error
	containers, err := GetDockerContainers(true)
	if err != nil {
		return []docker.APIContainers{{}}, err
	}
	containerMatches := []docker.APIContainers{}
	if len(labels) < 1 {
		return []docker.APIContainers{{}}, fmt.Errorf("the provided list of labels was empty")
	}

	// First, ensure a site name is set and matches the current application.
	for i := range containers {
		matched := true
		for matchName, matchValue := range labels {
			// If the label exists check the value to ensure a match
			if labelValue, ok := containers[i].Labels[matchName]; ok {
				if labelValue != matchValue {
					matched = false
					break
				}
			} else {
				// If the label does not exist, we can just fail immediately.
				matched = false
				break
			}
		}

		if matched {
			containerMatches = append(containerMatches, containers[i])
		}
	}

	return containerMatches, returnError
}

// NetExists checks to see if the docker network for ddev exists.
func NetExists(client *docker.Client, name string) bool {
	nets, _ := client.ListNetworks()
	for _, n := range nets {
		if n.Name == name {
			return true
		}
	}
	return false
}

// ContainerWait provides a wait loop to check for container in "healthy" status.
// waittime is in seconds.
// This is modeled on https://gist.github.com/ngauthier/d6e6f80ce977bedca601
// Returns logoutput, error, returns error if not "healthy"
func ContainerWait(waittime int, labels map[string]string) (string, error) {

	timeoutChan := time.After(time.Duration(waittime) * time.Second)
	tickChan := time.NewTicker(500 * time.Millisecond)
	defer tickChan.Stop()

	status := ""

	for {
		select {
		case <-timeoutChan:
			return "", fmt.Errorf("health check timed out: labels %v timed out without becoming healthy, status=%v", labels, status)

		case <-tickChan.C:
			container, err := FindContainerByLabels(labels)
			if err != nil || container == nil {
				return "", fmt.Errorf("failed to query container labels=%v: %v", labels, err)
			}
			status, logOutput := GetContainerHealth(container)

			switch status {
			case "healthy":
				return logOutput, nil
			case "unhealthy":
				return logOutput, fmt.Errorf("container %s unhealthy: %s", container.Names[0], logOutput)
			case "exited":
				service := container.Labels["com.docker.compose.service"]
				return logOutput, fmt.Errorf("container exited, please use 'ddev logs -s %s` to find out why it failed", service)
			}
		}
	}

	// We should never get here.
	//nolint: govet
	return "", fmt.Errorf("inappropriate break out of for loop in ContainerWait() waiting for container labels %v", labels)
}

// ContainerWaitLog provides a wait loop to check for container in "healthy" status.
// with a given log output
// timeout is in seconds.
// This is modeled on https://gist.github.com/ngauthier/d6e6f80ce977bedca601
// Returns logoutput, error, returns error if not "healthy"
func ContainerWaitLog(waittime int, labels map[string]string, expectedLog string) (string, error) {

	timeoutChan := time.After(time.Duration(waittime) * time.Second)
	tickChan := time.NewTicker(500 * time.Millisecond)
	defer tickChan.Stop()

	status := ""

	for {
		select {
		case <-timeoutChan:
			return "", fmt.Errorf("health check timed out: labels %v timed out without becoming healthy, status=%v", labels, status)

		case <-tickChan.C:
			container, err := FindContainerByLabels(labels)
			if err != nil || container == nil {
				return "", fmt.Errorf("failed to query container labels=%v: %v", labels, err)
			}
			status, logOutput := GetContainerHealth(container)

			switch {
			case status == "healthy" && expectedLog == logOutput:
				return logOutput, nil
			case status == "unhealthy":
				return logOutput, fmt.Errorf("container %s unhealthy: %s", container.Names[0], logOutput)
			case status == "exited":
				service := container.Labels["com.docker.compose.service"]
				return logOutput, fmt.Errorf("container exited, please use 'ddev logs -s %s` to find out why it failed", service)
			}
		}
	}

	// We should never get here.
	//nolint: govet
	return "", fmt.Errorf("inappropriate break out of for loop in ContainerWaitLog() waiting for container labels %v", labels)
}

// ContainerName returns the containers human readable name.
func ContainerName(container docker.APIContainers) string {
	return container.Names[0][1:]
}

// GetContainerHealth retrieves the health status of a given container.
// returns status, most-recent-log
func GetContainerHealth(container *docker.APIContainers) (string, string) {
	if container == nil {
		return "no container", ""
	}

	// If the container is not running, then return exited as the health.
	// "exited" means stopped.
	if container.State == "exited" || container.State == "restarting" {
		return container.State, ""
	}

	client := GetDockerClient()
	inspect, err := client.InspectContainer(container.ID)
	if err != nil || inspect == nil {
		output.UserOut.Warnf("Error getting container to inspect: %v", err)
		return "", ""
	}

	logOutput := ""
	status := inspect.State.Health.Status
	// The last log is the most recent
	if len(inspect.State.Health.Log) > 0 {
		numLogs := len(inspect.State.Health.Log)
		logOutput = inspect.State.Health.Log[numLogs-1].Output
	}

	return status, logOutput
}

// ComposeWithStreams executes a docker-compose command but allows the caller to specify
// stdin/stdout/stderr
func ComposeWithStreams(composeFiles []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, action ...string) error {
	var arg []string

	for _, file := range composeFiles {
		arg = append(arg, "-f")
		arg = append(arg, file)
	}

	arg = append(arg, action...)

	proc := exec.Command("docker-compose", arg...)
	proc.Stdout = stdout
	proc.Stdin = stdin
	proc.Stderr = stderr

	err := proc.Run()
	return err
}

// ComposeCmd executes docker-compose commands via shell.
// returns stdout, stderr, error/nil
func ComposeCmd(composeFiles []string, action ...string) (string, string, error) {
	var arg []string
	var stdout bytes.Buffer
	var stderr string

	for _, file := range composeFiles {
		arg = append(arg, "-f")
		arg = append(arg, file)
	}

	arg = append(arg, action...)

	proc := exec.Command("docker-compose", arg...)
	proc.Stdout = &stdout
	proc.Stdin = os.Stdin

	stderrPipe, err := proc.StderrPipe()
	if err != nil {
		return "", "", fmt.Errorf("Failed to proc.StderrPipe(): %v", err)
	}

	if err = proc.Start(); err != nil {
		return "", "", fmt.Errorf("Failed to exec docker-compose: %v", err)
	}

	// read command's stdout line by line
	in := bufio.NewScanner(stderrPipe)

	for in.Scan() {
		line := in.Text()
		if len(stderr) > 0 {
			stderr = stderr + "\n"
		}
		stderr = stderr + line
		line = strings.Trim(line, "\n\r")
		output.UserOut.Println(line)
	}

	err = proc.Wait()
	if err != nil {
		return stdout.String(), stderr, fmt.Errorf("Failed to run docker-compose %v, err='%v', stdout='%s', stderr='%s'", arg, err, stdout.String(), stderr)
	}
	return stdout.String(), stderr, nil
}

// GetAppContainers retrieves docker containers for a given sitename.
func GetAppContainers(sitename string) ([]docker.APIContainers, error) {
	label := map[string]string{"com.ddev.site-name": sitename}
	sites, err := FindContainersByLabels(label)
	if err != nil {
		return sites, err
	}
	return sites, nil
}

// GetContainerEnv returns the value of a given environment variable from a given container.
func GetContainerEnv(key string, container docker.APIContainers) string {
	client := GetDockerClient()
	inspect, err := client.InspectContainer(container.ID)
	if err == nil {
		envVars := inspect.Config.Env

		for _, env := range envVars {
			if strings.HasPrefix(env, key) {
				return strings.TrimPrefix(env, key+"=")
			}
		}
	}
	return ""
}

// CheckDockerVersion determines if the docker version of the host system meets the provided version
// constraints. See https://godoc.org/github.com/Masterminds/semver#hdr-Checking_Version_Constraints
// for examples defining version constraints.
func CheckDockerVersion(versionConstraint string) error {
	currentVersion, err := version.GetDockerVersion()
	if err != nil {
		return fmt.Errorf("no docker")
	}
	dockerVersion, err := semver.NewVersion(currentVersion)
	if err != nil {
		return err
	}

	constraint, err := semver.NewConstraint(versionConstraint)
	if err != nil {
		return err
	}

	match, errs := constraint.Validate(dockerVersion)
	if !match {
		if len(errs) <= 1 {
			return errs[0]
		}

		msgs := "\n"
		for _, err := range errs {
			msgs = fmt.Sprint(msgs, err, "\n")
		}
		return fmt.Errorf(msgs)
	}
	return nil
}

// CheckDockerCompose determines if docker-compose is present and executable on the host system. This
// relies on docker-compose being somewhere in the user's $PATH.
func CheckDockerCompose(versionConstraint string) error {
	version, err := version.GetDockerComposeVersion()
	if err != nil {
		return err
	}
	dockerComposeVersion, err := semver.NewVersion(version)
	if err != nil {
		return err
	}

	constraint, err := semver.NewConstraint(versionConstraint)
	if err != nil {
		return err
	}

	match, errs := constraint.Validate(dockerComposeVersion)
	if !match {
		if len(errs) <= 1 {
			return errs[0]
		}

		msgs := "\n"
		for _, err := range errs {
			msgs = fmt.Sprint(msgs, err, "\n")
		}
		return fmt.Errorf(msgs)
	}

	return nil
}

// GetPublishedPort returns the published port for a given private port.
func GetPublishedPort(privatePort int64, container docker.APIContainers) int {
	for _, port := range container.Ports {
		if port.PrivatePort == privatePort {
			return int(port.PublicPort)
		}
	}
	return 0
}

// CheckForHTTPS determines if a container has the HTTPS_EXPOSE var
// set to route 443 traffic to 80
func CheckForHTTPS(container docker.APIContainers) bool {
	env := GetContainerEnv("HTTPS_EXPOSE", container)
	if env != "" && strings.Contains(env, "443:80") {
		return true
	}
	return false
}

// GetDockerIP returns either the default Docker IP address (127.0.0.1)
// or the value as configured by $DOCKER_HOST.
func GetDockerIP() (string, error) {
	dockerIP := "127.0.0.1"
	dockerHostRawURL := os.Getenv("DOCKER_HOST")
	if dockerHostRawURL != "" {
		dockerHostURL, err := url.Parse(dockerHostRawURL)
		if err != nil {
			return "", fmt.Errorf("failed to parse $DOCKER_HOST: %v, err: %v", dockerHostRawURL, err)
		}

		dockerIP = dockerHostURL.Hostname()
	}

	return dockerIP, nil
}

// RunSimpleContainer runs a container (non-daemonized) and captures the stdout.
// It will block, so not to be run on a container whose entrypoint or cmd might hang or run too long.
// This should be the equivalent of something like
// docker run -t -u '%s:%s' -e SNAPSHOT_NAME='%s' -v '%s:/mnt/ddev_config' -v '%s:/var/lib/mysql' --rm --entrypoint=/migrate_file_to_volume.sh %s:%s"
// Example code from https://gist.github.com/fsouza/b0bf3043827f8e39c4589e88cec067d8
// Returns containerID, output, error
func RunSimpleContainer(image string, name string, cmd []string, entrypoint []string, env []string, binds []string, uid string, removeContainerAfterRun bool) (containerID string, output string, returnErr error) {
	client := GetDockerClient()

	// Ensure image string includes a tag
	imageChunks := strings.Split(image, ":")
	if len(imageChunks) == 1 {
		// Image does not specify tag
		return "", "", fmt.Errorf("image name must specify tag: %s", image)
	}

	if tag := imageChunks[len(imageChunks)-1]; len(tag) == 0 {
		// Image specifies malformed tag (ends with ':')
		return "", "", fmt.Errorf("malformed tag provided: %s", image)
	}

	existsLocally, err := ImageExistsLocally(image)
	if err != nil {
		return "", "", fmt.Errorf("failed to check if image %s is available locally: %v", image, err)
	}

	if !existsLocally {
		var buf bytes.Buffer
		pullErr := client.PullImage(docker.PullImageOptions{Repository: image, OutputStream: &buf},
			docker.AuthConfiguration{})
		if pullErr != nil {
			return "", "", fmt.Errorf("failed to pull image %s: %v", image, pullErr)
		}
	}

	// Windows 10 Docker toolbox won't handle a bind mount like C:\..., so must convert to /c/...
	if runtime.GOOS == "windows" {
		for i := range binds {
			binds[i] = strings.Replace(binds[i], `\`, `/`, -1)
			if strings.Index(binds[i], ":") == 1 {
				binds[i] = strings.Replace(binds[i], ":", "", 1)
				binds[i] = "/" + binds[i]
				// And amazingly, the drive letter must be lower-case.
				re := regexp.MustCompile("^/[A-Z]/")
				driveLetter := re.FindString(binds[i])
				if len(driveLetter) == 3 {
					binds[i] = strings.TrimPrefix(binds[i], driveLetter)
					binds[i] = strings.ToLower(driveLetter) + binds[i]
				}

			}
		}
	}

	options := docker.CreateContainerOptions{
		Name: name,
		Config: &docker.Config{
			Image:        image,
			Cmd:          cmd,
			Env:          env,
			User:         uid,
			Entrypoint:   entrypoint,
			AttachStderr: true,
			AttachStdout: true,
		},
		HostConfig: &docker.HostConfig{
			Binds: binds,
		},
	}

	container, err := client.CreateContainer(options)
	if err != nil {
		return "", "", fmt.Errorf("failed to create/start docker container (%v):%v", options, err)
	}

	if removeContainerAfterRun {
		// nolint: errcheck
		defer RemoveContainer(container.ID, 20)
	}
	err = client.StartContainer(container.ID, nil)
	if err != nil {
		return container.ID, "", fmt.Errorf("failed to StartContainer: %v", err)
	}
	exitCode, err := client.WaitContainer(container.ID)
	if err != nil {
		return container.ID, "", fmt.Errorf("failed to WaitContainer: %v", err)
	}

	// Get logs so we can report them if exitCode failed
	var stdout bytes.Buffer
	err = client.Logs(docker.LogsOptions{
		Stdout:       true,
		Stderr:       true,
		Container:    container.ID,
		OutputStream: &stdout,
	})
	if err != nil {
		return container.ID, "", fmt.Errorf("failed to get Logs(): %v", err)
	}

	// This is the exitCode from the client.WaitContainer()
	if exitCode != 0 {
		return container.ID, stdout.String(), fmt.Errorf("container run failed with exit code %d", exitCode)
	}

	return container.ID, stdout.String(), nil
}

// RemoveContainer stops and removes a container
func RemoveContainer(id string, timeout uint) error {
	client := GetDockerClient()

	err := client.RemoveContainer(docker.RemoveContainerOptions{ID: id, Force: true})
	return err
}

// ImageExistsLocally determines if an image is available locally.
func ImageExistsLocally(imageName string) (bool, error) {
	client := GetDockerClient()

	images, err := client.ListImages(docker.ListImagesOptions{
		Filter: imageName,
	})

	if err != nil {
		return false, err
	}

	if len(images) == 0 {
		return false, nil
	}

	for _, i := range images {
		// RepoTags is a slice in the format of <repo-name>:<tag>, like drud/ddev-webserver:v1.2.3
		for _, tag := range i.RepoTags {
			if tag == imageName {
				return true, nil
			}
		}
	}

	return false, nil
}

// Pull pulls image if it doesn't exist locally.
func Pull(imageName string) error {
	exists, err := ImageExistsLocally(imageName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	cmd := exec.Command("docker", "pull", imageName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	return err
}

// GetExposedContainerPorts takes a container pointer and returns an array
// of exposed ports (and error)
func GetExposedContainerPorts(containerID string) ([]string, error) {
	client := GetDockerClient()
	inspectInfo, err := client.InspectContainer(containerID)

	if err != nil {
		return nil, err
	}

	ports := []string{}
	for _, portMapping := range inspectInfo.NetworkSettings.Ports {
		if portMapping != nil && len(portMapping) > 0 {
			for _, item := range portMapping {
				ports = append(ports, item.HostPort)
			}
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		return ports[i] < ports[j]
	})
	return ports, nil
}

// MassageWindowsHostMountpoint changes C:/path/to/something to //c/path/to/something
// THis is required for docker bind mounts on docker toolbox.
// Sadly, if we have a Windows drive name, it has to be converted from C:/ to //c for Win10Home/Docker toolbox
func MassageWindowsHostMountpoint(mountPoint string) string {
	if string(mountPoint[1]) == ":" {
		pathPortion := strings.Replace(mountPoint[2:], `\`, "/", -1)
		drive := strings.ToLower(string(mountPoint[0]))
		mountPoint = "//" + drive + pathPortion
	}
	return mountPoint
}

// MassageWindowsNFSMount changes C:\Path\to\something to /c/Path/to/something
func MassageWindowsNFSMount(mountPoint string) string {
	if string(mountPoint[1]) == ":" {
		pathPortion := strings.Replace(mountPoint[2:], `\`, "/", -1)
		drive := string(mountPoint[0])
		// Because we use $HOME to get home in exports, and $HOME has /c/Users/xxx
		// change the drive to lower case.
		mountPoint = "/" + strings.ToLower(drive) + pathPortion
	}
	return mountPoint
}

// RemoveVolume removes named volume. Does not throw error if the volume did not exist.
func RemoveVolume(volumeName string) error {
	client := GetDockerClient()
	err := client.RemoveVolumeWithOptions(docker.RemoveVolumeOptions{Name: volumeName})
	if err != nil && err.Error() != "" && err.Error() != "no such volume" {
		return err
	}
	return nil
}

// CreateVolume creates a docker volume
func CreateVolume(volumeName string, driver string, driverOpts map[string]string) (volume *docker.Volume, err error) {
	client := GetDockerClient()
	volume, err = client.CreateVolume(docker.CreateVolumeOptions{Name: volumeName, Driver: driver, DriverOpts: driverOpts})
	return volume, err
}

// GetHostDockerInternalIP() returns either "host.docker.internal"
// (for docker-for-mac and Win10 Docker-for-windows) or a usable IP address
// for docker toolbox and linux.
func GetHostDockerInternalIP() (string, error) {
	hostDockerInternal := ""

	// Docker 18.09 on linux and docker-toolbox don't define host.docker.internal
	// so we need to go get the ip address of docker0
	// We would hope to be able to remove this when
	// https://github.com/docker/for-linux/issues/264 gets resolved.
	if runtime.GOOS == "linux" {
		out, err := exec2.RunCommandPipe("ip", []string{"address", "show", "dev", "docker0"})
		// Do not process if ip command fails, we'll just ignore and not act.
		if err == nil {
			addr := regexp.MustCompile(`inet *[0-9\.]+`).FindString(out)
			components := strings.Split(addr, " ")
			if len(components) == 2 {
				hostDockerInternal = components[1]
			} else {
				return "", fmt.Errorf("docker0 interface IP address cannot be determined. You may need to 'ip link set docker0 up' or restart docker or reboot to get xdebug or nfsmount_enabled to work")
			}
		}
	} else if nodeps.IsDockerToolbox() {
		dockerIP, err := GetDockerIP()
		if err != nil {
			return "", err
		}
		octets := strings.Split(dockerIP, ".")
		if len(octets) != 4 {
			return "", fmt.Errorf("dockerIP %s does not have 4 octets", dockerIP)
		}
		// If the docker IP is 192.168.99.100, the *router* ip is 192.168.99.1
		// So replace the final octet with 1.
		hostDockerInternal = fmt.Sprintf("%s.%s.%s.1", octets[0], octets[1], octets[2])
	}
	return hostDockerInternal, nil
}
