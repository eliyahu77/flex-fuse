package flex

import (
	"encoding/json"
	"fmt"
	"github.com/v3io/flex-fuse/pkg/cri"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/v3io/flex-fuse/pkg/journal"
)

type Mounter struct {
	Config *Config
}

func NewMounter() (*Mounter, error) {
	journal.Debug("Creating configuration")
	config, err := NewConfig()
	if err != nil {
		return nil, err
	}

	return &Mounter{
		Config: config,
	}, nil
}

func (m *Mounter) Mount(targetPath string, specString string) *Response {
	journal.Debug("Mounting")

	spec := Spec{}
	if err := json.Unmarshal([]byte(specString), &spec); err != nil {
		return NewFailResponse("Failed to unmarshal spec", err)
	}

	if err := spec.validate(); err != nil {
		return NewFailResponse("Mount failed validation", err)
	}

	if m.Config.Type == "link" {
		return m.mountAsLink(&spec, targetPath)
	}

	if isMountPoint(targetPath) {
		return NewSuccessResponse(fmt.Sprintf("Already mounted: %s", targetPath))
	}

	if err := m.createV3IOFUSEContainer(&spec, targetPath); err != nil {
		return NewFailResponse("Failed to create v3io FUSE container", err)
	}

	if err := m.createDirs(spec, targetPath); err != nil {
		return NewFailResponse("Failed to create folders", err)
	}

	return NewSuccessResponse("Successfully mounted")
}

func (m *Mounter) createDirs(spec Spec, targetPath string) error {
	var dirsToCreate []DirToCreate
	if err := json.Unmarshal([]byte(spec.DirsToCreate), &dirsToCreate); err != nil {
		if spec.DirsToCreate != "" {
			return fmt.Errorf("Failed to parse dirsToCreate [%s]: %s", spec.DirsToCreate, err.Error())
		}
		return nil
	}
	for _, dir := range dirsToCreate {
		if strings.HasPrefix(dir.Name, "/") {
			return fmt.Errorf("Only creation of relative path is supported (%s)", dir.Name)
		}
		dirToCreate := fmt.Sprintf("%s/%s", targetPath, dir.Name)

		_, err := os.Stat(dirToCreate)
		if err == nil {
			journal.Debug(fmt.Sprintf("Folder already exists: %s", dirToCreate))
			continue
		}

		if !os.IsNotExist(err) {
			return fmt.Errorf("Stat failed for folder [%s]: %s", dirToCreate, err)
		}

		if err := os.MkdirAll(dirToCreate, dir.Permissions); err != nil {
			return fmt.Errorf("Failed to create folder (path: %s, filemode: %o): %s", dir.Name, dir.Permissions, err.Error())
		}
		journal.Debug(fmt.Sprintf("Created folder: %s", dirToCreate))
	}
	return nil
}

func (m *Mounter) Unmount(targetPath string) *Response {
	journal.Debug("Unmounting", "targetPath", targetPath)

	if m.Config.Type == "link" {
		return m.unmountAsLink(targetPath)
	}

	if !isMountPoint(targetPath) {
		return NewSuccessResponse(fmt.Sprintf("%s Not a mountpoint, nothing to do", targetPath))
	}

	criInstance, err := createCRI()
	if err != nil {
		return NewFailResponse("Failed to create CRI", err)
	}

	defer criInstance.Close() // nolint: errcheck

	if err := m.removeV3IOFUSEContainer(criInstance, targetPath); err != nil {
		return NewFailResponse("Failed to remove v3io FUSE container", err)
	}

	journal.Info("Unmounting target path with umount", "target", targetPath)

	umountCommand := exec.Command("umount", targetPath)
	if err := umountCommand.Start(); err != nil {
		return NewFailResponse("Failed to call unmount", err)
	}

	for _, interval := range []time.Duration{1, 2, 4} {
		if !isMountPoint(targetPath) {

			// once unmounted, remove it
			if err := os.Remove(targetPath); err != nil {
				return NewFailResponse(fmt.Sprintf("Could not remove directory %s", targetPath), err)
			}

			return NewSuccessResponse("Successfully unmounted")
		}

		time.Sleep(interval * time.Second)
	}

	return NewFailResponse(fmt.Sprintf("Failed to umount %s due to timeout", targetPath), nil)
}

func (m *Mounter) createV3IOFUSEContainer(spec *Spec, targetPath string) error {
	journal.Info("Creating v3io-fuse container", "target", targetPath)

	criInstance, err := createCRI()
	if err != nil {
		return err
	}

	defer criInstance.Close() // nolint: errcheck

	ImageRepository := m.Config.ImageRepository
	if ImageRepository == "" {
		ImageRepository = "iguazio/v3io-fuse"
	}

	ImageTag := m.Config.ImageTag
	if ImageTag == "" {
		ImageTag = "local"
	}

	dataUrls, err := m.Config.DataURLs(spec.GetClusterName())
	if err != nil {
		return fmt.Errorf("Could not get cluster data urls: %s", err.Error())
	}

	containerName, err := getContainerNameFromTargetPath(targetPath)
	if err != nil {
		return fmt.Errorf("Failed to get container name: %s", err.Error())
	}

	// Ensure the container doesn't already exist
	// It's ok if the command runs but exits with a failure, this is in the case the container doesn't exist.
	m.removeV3IOFUSEContainer(criInstance, targetPath) // nolint: errcheck

	// Create the new container
	args := []string{
		"/fuse/mounter.sh",
		"-o", "allow_other",
		"--connection_strings", dataUrls,
		"--mountpoint", "/fuse_mount",
		"--session_key", spec.GetAccessKey(),
	}

	V3ioConfigPath := m.Config.V3ioConfigPath
	if V3ioConfigPath != "" {
		args = append(args, "-f", V3ioConfigPath)
	}

	if spec.Container != "" {
		containerBackslashEncoded := "\\" + strings.Join(strings.Split(spec.Container, ""), "\\")
		args = append(args, "-a", containerBackslashEncoded)
		if spec.SubPath != "" {
			subPathBackslashEncoded := "\\" + strings.Join(strings.Split(spec.SubPath, ""), "\\")
			args = append(args, "-p", subPathBackslashEncoded)
		}
	}

	if err := criInstance.CreateContainer(fmt.Sprintf("%s:%s", ImageRepository, ImageTag),
		containerName,
		targetPath,
		args); err != nil {
		return fmt.Errorf("Failed to create container for %s: %s", targetPath, err)
	}

	for _, interval := range []time.Duration{1, 2, 4, 2, 1} {
		if isMountPoint(targetPath) {
			return nil
		}

		time.Sleep(interval * time.Second)
	}

	return fmt.Errorf("Failed to mount %s due to timeout", targetPath)
}

func (m *Mounter) removeV3IOFUSEContainer(criInstance cri.CRI, targetPath string) error {
	journal.Info("Removing v3io-fuse container", "target", targetPath)

	containerName, err := getContainerNameFromTargetPath(targetPath)
	if err != nil {
		return fmt.Errorf("Could not get container name: %s", err)
	}

	if err := criInstance.RemoveContainer(containerName); err != nil {
		return fmt.Errorf("Could not remove container for %s: %s", targetPath, err)
	}

	journal.Debug("Container removed", "containerName", containerName)

	return nil
}

func (m *Mounter) mountAsLink(spec *Spec, targetPath string) *Response {
	journal.Info("Mounting as link", "target", targetPath)
	linkPath := path.Join("/mnt/v3io", spec.Namespace, spec.Container)

	if !isMountPoint(linkPath) {
		journal.Debug("Creating folder", "linkPath", linkPath)
		if err := os.MkdirAll(linkPath, 0755); err != nil {
			return NewFailResponse(fmt.Sprintf("Failed to create target %s", linkPath), err)
		}

		if err := m.createV3IOFUSEContainer(spec, linkPath); err != nil {
			return NewFailResponse("Failed to create v3io FUSE container", err)
		}
	}

	if err := os.Remove(targetPath); err != nil {
		return NewFailResponse(fmt.Sprintf("Failed to remove target %s", targetPath), err)
	}

	if err := os.Symlink(linkPath, targetPath); err != nil {
		return NewFailResponse(fmt.Sprintf("Failed to create link %s to target %s", linkPath, targetPath), err)
	}

	return NewSuccessResponse("Successfully mounted as link")
}

func (m *Mounter) unmountAsLink(targetPath string) *Response {
	journal.Info("Calling unmountAsLink command", "target", targetPath)
	if err := os.Remove(targetPath); err != nil {
		return NewFailResponse("unable to remove link", err)
	}

	return NewSuccessResponse("link removed")
}

// /var/lib/kubelet/pods/0c082652-d6c7-11e9-9fd4-a4bf015abcab/volumes/v3io~fuse/v3io-fuse -> "v3io-fuse-0c082652-d6c7-11e9-9fd4-a4bf015abcab-v3io-fuse
func getContainerNameFromTargetPath(targetPath string) (string, error) {
	splitTargetPath := strings.Split(targetPath, string(filepath.Separator))

	for targetPathPartIdx, targetPathPart := range splitTargetPath {

		// if we found the pods part, return the part after it - if not at the end
		if targetPathPart == "pods" {
			podIDIdx := targetPathPartIdx + 1

			if podIDIdx >= len(splitTargetPath) {
				return "", fmt.Errorf("Expected a directory after pods, found it at the end: %s", targetPath)

			}

			// v3io-fuse-<pod id>-<last part of path, which is the volume name>
			return fmt.Sprintf("v3io-fuse-%s-%s", splitTargetPath[podIDIdx], splitTargetPath[len(splitTargetPath)-1]), nil
		}
	}

	return "", fmt.Errorf("Could not find pod directory in path: %s", targetPath)
}

func isMountPoint(path string) bool {
	journal.Debug("Checking if path is a mount point", "target", path)

	cmd := exec.Command("mount")
	mountList, err := cmd.CombinedOutput()
	if err != nil {
		journal.Debug("Path is not a mount point", "target", path)
		return false
	}

	mountListString := string(mountList)
	result := strings.Contains(mountListString, path+" type")

	if result {
		journal.Debug("Path is a mount point", "target", path)
	} else {
		journal.Debug("Path is not a mount point", "target", path)
	}

	return result
}

func createCRI() (cri.CRI, error) {
	dockerBinaryPath := "/usr/bin/docker"

	// if docker binary exists, create docker. otherwise containerd
	if _, err := os.Stat(dockerBinaryPath); os.IsNotExist(err) {
		return cri.NewContainerd("/run/containerd/containerd.sock", "v3io")
	}

	return cri.NewDocker(dockerBinaryPath)
}
