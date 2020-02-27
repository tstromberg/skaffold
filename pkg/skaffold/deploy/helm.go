/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deploy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/blang/semver"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/event"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/runcontext"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/warnings"
)

// HelmDeployer deploys workflows using the helm CLI
type HelmDeployer struct {
	*latest.HelmDeploy

	kubeContext string
	kubeConfig  string
	namespace   string
	forceDeploy bool

	// packaging temporary directory, used for predictable test output
	pkgTmpDir string

	// bV is the helm binary version
	bV semver.Version
}

// NewHelmDeployer returns a configured HelmDeployer
func NewHelmDeployer(runCtx *runcontext.RunContext) *HelmDeployer {
	return &HelmDeployer{
		HelmDeploy:  runCtx.Cfg.Deploy.HelmDeploy,
		kubeContext: runCtx.KubeContext,
		kubeConfig:  runCtx.Opts.KubeConfig,
		namespace:   runCtx.Opts.Namespace,
		forceDeploy: runCtx.Opts.Force,
	}
}

// Labels returns the Kubernetes labels used by this deployer
func (h *HelmDeployer) Labels() map[string]string {
	return map[string]string{
		constants.Labels.Deployer: "helm",
	}
}

// Deploy deploys the build results to the Kubernetes cluster
func (h *HelmDeployer) Deploy(ctx context.Context, out io.Writer, builds []build.Artifact, labellers []Labeller) *Result {
	event.DeployInProgress()

	hv, err := h.binVer(ctx)
	if err != nil {
		logrus.Debugf("failed to parse binary version: %v", err)
	} else {
		logrus.Debugf("deploying with helm version %v", hv)
	}

	var dRes []Artifact
	nsMap := map[string]struct{}{}
	valuesSet := map[string]bool{}

	// Deploy every release
	for _, r := range h.Releases {
		results, err := h.deployRelease(ctx, out, r, builds, valuesSet)
		if err != nil {
			releaseName, _ := expand(r.Name, nil)

			event.DeployFailed(err)
			return NewDeployErrorResult(errors.Wrapf(err, "deploying %s", releaseName))
		}

		// collect namespaces
		for _, r := range results {
			if trimmed := strings.TrimSpace(r.Namespace); trimmed != "" {
				nsMap[trimmed] = struct{}{}
			}
		}

		dRes = append(dRes, results...)
	}

	// Let's make sure that every image tag is set with `--set`.
	// Otherwise, templates have no way to use the images that were built.
	for _, build := range builds {
		if !valuesSet[build.Tag] {
			warnings.Printf("image [%s] is not used.", build.Tag)
			warnings.Printf("image [%s] is used instead.", build.ImageName)
			warnings.Printf("See helm sample for how to replace image names with their actual tags: https://github.com/GoogleContainerTools/skaffold/blob/master/examples/helm-deployment/skaffold.yaml")
		}
	}

	event.DeployComplete()

	labels := merge(h, labellers...)
	labelDeployResults(labels, dRes)

	// Collect namespaces in a string
	namespaces := make([]string, 0, len(nsMap))
	for ns := range nsMap {
		namespaces = append(namespaces, ns)
	}

	return NewDeploySuccessResult(namespaces)
}

// Dependencies returns a list of files that the deployer depends on.
func (h *HelmDeployer) Dependencies() ([]string, error) {
	var deps []string

	for _, release := range h.Releases {
		r := release
		deps = append(deps, r.ValuesFiles...)

		if r.Remote {
			// chart path is only a dependency if it exists on the local filesystem
			continue
		}

		chartDepsDir := filepath.Join(release.ChartPath, "charts")
		err := filepath.Walk(release.ChartPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return errors.Wrapf(err, "failure accessing path '%s'", path)
			}

			if !info.IsDir() {
				if !strings.HasPrefix(path, chartDepsDir) || r.SkipBuildDependencies {
					// We can always add a dependency if it is not contained in our chartDepsDir.
					// However, if the file is in  our chartDepsDir, we can only include the file
					// if we are not running the helm dep build phase, as that modifies files inside
					// the chartDepsDir and results in an infinite build loop.
					deps = append(deps, path)
				}
			}

			return nil
		})

		if err != nil {
			return deps, errors.Wrap(err, "issue walking releases")
		}
	}
	sort.Strings(deps)
	return deps, nil
}

// Cleanup deletes what was deployed by calling Deploy.
func (h *HelmDeployer) Cleanup(ctx context.Context, out io.Writer) error {
	for _, r := range h.Releases {
		releaseName, err := expand(r.Name, nil)
		if err != nil {
			return errors.Wrap(err, "cannot parse the release name template")
		}

		if err := h.exec(ctx, out, false, "delete", releaseName, "--purge"); err != nil {
			return errors.Wrapf(err, "deleting %s", releaseName)
		}
	}
	return nil
}

// Render generates the Kubernetes manifests and writes them out
func (h *HelmDeployer) Render(context.Context, io.Writer, []build.Artifact, []Labeller, string) error {
	return errors.New("not yet implemented")
}

// exec executes the helm command, writing combined stdout/stderr to the provided writer
func (h *HelmDeployer) exec(ctx context.Context, out io.Writer, useSecrets bool, args ...string) error {
	if args[0] != "version" {
		args = append([]string{"--kube-context", h.kubeContext}, args...)
		args = append(args, h.Flags.Global...)

		if h.kubeConfig != "" {
			args = append(args, "--kubeconfig", h.kubeConfig)
		}

		if useSecrets {
			args = append([]string{"secrets"}, args...)
		}
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = out
	cmd.Stderr = out

	return util.RunCmd(cmd)
}

// deployRelease deploys a single release
func (h *HelmDeployer) deployRelease(ctx context.Context, out io.Writer, r latest.HelmRelease, builds []build.Artifact, valuesSet map[string]bool) ([]Artifact, error) {
	releaseName, err := expand(r.Name, nil)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse the release name template")
	}

	opts := installOpts{
		releaseName: releaseName,
		upgrade:     true,
		flags:       h.Flags.Upgrade,
		force:       h.forceDeploy,
		chartPath:   r.ChartPath,
	}

	if err := h.exec(ctx, ioutil.Discard, false, getArgs(releaseName)...); err != nil {
		color.Yellow.Fprintf(out, "Helm release %s not installed. Installing...\n", releaseName)

		opts.upgrade = false
		opts.flags = h.Flags.Install
	}

	if h.namespace != "" {
		opts.namespace = h.namespace
	} else if r.Namespace != "" {
		opts.namespace = r.Namespace
	}

	// Only build local dependencies, but allow a user to skip them.
	if !r.SkipBuildDependencies && !r.Remote {
		logrus.Infof("Building helm dependencies...")

		if err := h.exec(ctx, out, false, "dep", "build", r.ChartPath); err != nil {
			return nil, errors.Wrap(err, "building helm dependencies")
		}
	}

	// Dump overrides to a YAML file to pass into helm
	if len(r.Overrides.Values) != 0 {
		overrides, err := yaml.Marshal(r.Overrides)
		if err != nil {
			return nil, errors.Wrap(err, "cannot marshal overrides to create overrides values.yaml")
		}

		if err := ioutil.WriteFile(constants.HelmOverridesFilename, overrides, 0666); err != nil {
			return nil, errors.Wrapf(err, "cannot create file %s", constants.HelmOverridesFilename)
		}

		defer func() {
			os.Remove(constants.HelmOverridesFilename)
		}()
	}

	if r.Packaged != nil {
		chartPath, err := h.packageChart(ctx, r)
		if err != nil {
			return nil, errors.WithMessage(err, "cannot package chart")
		}

		opts.chartPath = chartPath
	}

	args, err := installArgs(r, builds, valuesSet, opts)
	if err != nil {
		return nil, errors.Wrap(err, "release args")
	}

	iErr := h.exec(ctx, out, r.UseHelmSecrets, args...)

	var b bytes.Buffer

	// Be accepting of failure
	if err := h.exec(ctx, &b, false, getArgs(releaseName)...); err != nil {
		logrus.Warnf(err.Error())
		return nil, nil
	}

	artifacts := parseReleaseInfo(opts.namespace, bufio.NewReader(&b))
	return artifacts, iErr
}

// binVer returns the version of the helm binary found in PATH. May be cached.
func (h *HelmDeployer) binVer(ctx context.Context) (semver.Version, error) {
	// Return the cached version value if non-zero
	if h.bV.Major != 0 && h.bV.Minor != 0 {
		return h.bV, nil
	}

	var b bytes.Buffer
	if err := h.exec(ctx, &b, false, "version", "--short", "-c"); err != nil {
		return semver.Version{}, errors.Wrap(err, "helm version")
	}
	bs := b.Bytes()

	// raw for 3.1: "v3.1.0+gb29d20b"
	// raw for 2.15: "Client: v2.15.1+gcf1de4f"
	raw := string(bs)
	idx := strings.Index(raw, "v")
	if idx < 0 {
		return semver.Version{}, fmt.Errorf("v not found in output: %q", raw)
	}

	// Only read up to a + sign if provided: semver does not understand + notation.
	rv := strings.Split(raw[idx+1:], "+")[0]
	v, err := semver.Make(rv)
	if err != nil {
		return semver.Version{}, errors.Wrap(err, "semver make")
	}

	h.bV = v
	return h.bV, nil
}

// installOpts are options to be passed to "helm install"
type installOpts struct {
	flags       []string
	releaseName string
	namespace   string
	chartPath   string
	upgrade     bool
	force       bool
}

// installArgs calculates the correct arguments to "helm install"
func installArgs(r latest.HelmRelease, builds []build.Artifact, valuesSet map[string]bool, o installOpts) ([]string, error) {
	var args []string
	if o.upgrade {
		args = append(args, "upgrade", o.releaseName)
		args = append(args, o.flags...)

		if o.force {
			args = append(args, "--force")
		}

		if r.RecreatePods {
			args = append(args, "--recreate-pods")
		}
	} else {
		args = append(args, "install", "--name", o.releaseName)
		args = append(args, o.flags...)
	}

	// There are 2 strategies:
	// 1) Deploy chart directly from filesystem path or from repository
	//    (like stable/kubernetes-dashboard). Version only applies to a
	//    chart from repository.
	// 2) Package chart into a .tgz archive with specific version and then deploy
	//    that packaged chart. This way user can apply any version and appVersion
	//    for the chart.
	if r.Packaged == nil && r.Version != "" {
		args = append(args, "--version", r.Version)
	}

	args = append(args, o.chartPath)

	if o.namespace != "" {
		args = append(args, "--namespace", o.namespace)
	}

	params, err := pairParamsToArtifacts(builds, r.Values)
	if err != nil {
		return nil, errors.Wrap(err, "matching build results to chart values")
	}

	if len(r.Overrides.Values) != 0 {
		args = append(args, "-f", constants.HelmOverridesFilename)
	}

	for k, v := range params {
		var value string

		cfg := r.ImageStrategy.HelmImageConfig.HelmConventionConfig

		value, err = imageSetFromConfig(cfg, k, v.Tag)
		if err != nil {
			return nil, err
		}

		valuesSet[v.Tag] = true
		args = append(args, "--set-string", value)
	}

	sortedKeys := make([]string, 0, len(r.SetValues))
	for k := range r.SetValues {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		valuesSet[r.SetValues[k]] = true
		args = append(args, "--set", fmt.Sprintf("%s=%s", k, r.SetValues[k]))
	}

	for k, v := range r.SetFiles {
		valuesSet[v] = true
		args = append(args, "--set-file", fmt.Sprintf("%s=%s", k, v))
	}

	envMap := map[string]string{}
	for idx, b := range builds {
		suffix := ""
		if idx > 0 {
			suffix = strconv.Itoa(idx + 1)
		}

		for k, v := range envVarForImage(b.ImageName, b.Tag) {
			envMap[k+suffix] = v
		}
	}
	logrus.Debugf("EnvVarMap: %+v\n", envMap)

	sortedKeys = make([]string, 0, len(r.SetValueTemplates))
	for k := range r.SetValueTemplates {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		v, err := expand(r.SetValueTemplates[k], envMap)
		if err != nil {
			return nil, err
		}

		valuesSet[v] = true
		args = append(args, "--set", fmt.Sprintf("%s=%s", k, v))
	}

	for _, v := range r.ValuesFiles {
		exp, err := homedir.Expand(v)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to expand %s", v)
		}

		exp, err = expand(exp, envMap)
		if err != nil {
			return nil, err
		}

		args = append(args, "-f", exp)
	}

	if r.Wait {
		args = append(args, "--wait")
	}

	return args, nil
}

// getArgs calculates the correct arguments to "helm get"
func getArgs(releaseName string) []string {
	return []string{"get", releaseName}
}

// envVarForImage creates an environment map for an image and digest tag (fqn)
func envVarForImage(imageName string, digest string) map[string]string {
	customMap := map[string]string{
		"IMAGE_NAME": imageName,
		"DIGEST":     digest, // The `DIGEST` name is kept for compatibility reasons
	}

	if digest == "" {
		return customMap
	}

	// DIGEST_ALGO and DIGEST_HEX are deprecated and will contain nonsense values
	names := strings.SplitN(digest, ":", 2)
	if len(names) >= 2 {
		customMap["DIGEST_ALGO"] = names[0]
		customMap["DIGEST_HEX"] = names[1]
	} else {
		customMap["DIGEST_HEX"] = digest
	}
	return customMap
}

// packageChart packages the chart and returns path to the chart archive file.
func (h *HelmDeployer) packageChart(ctx context.Context, r latest.HelmRelease) (string, error) {
	// Allow a test to sneak a predictable path in
	tmpDir := h.pkgTmpDir
	if tmpDir == "" {
		// Guarantee a unique path to avoid toctou bugs
		t, err := ioutil.TempDir("", "skaffold-helm")
		if err != nil {
			return "", errors.Wrap(err, "tempdir")
		}
		tmpDir = t
	}

	packageArgs := []string{"package", r.ChartPath, "--destination", tmpDir}

	if r.Packaged.Version != "" {
		v, err := expand(r.Packaged.Version, nil)
		if err != nil {
			return "", errors.Wrap(err, `concretize "packaged.version" template`)
		}
		packageArgs = append(packageArgs, "--version", v)
	}

	if r.Packaged.AppVersion != "" {
		av, err := expand(r.Packaged.AppVersion, nil)
		if err != nil {
			return "", errors.Wrap(err, `concretize "packaged.appVersion" template`)
		}
		packageArgs = append(packageArgs, "--app-version", av)
	}

	buf := &bytes.Buffer{}

	if err := h.exec(ctx, buf, false, packageArgs...); err != nil {
		return "", errors.Wrapf(err, "package chart into a .tgz archive: %v", packageArgs)
	}

	output := strings.TrimSpace(buf.String())

	idx := strings.Index(output, tmpDir)
	if idx == -1 {
		return "", errors.New("cannot locate packaged chart archive")
	}

	fpath := output[idx+len(tmpDir):]
	return filepath.Join(tmpDir, fpath), nil
}

// imageSetFromConfig calculates the --set-string value from the helm config
func imageSetFromConfig(cfg *latest.HelmConventionConfig, valueName string, tag string) (string, error) {
	if cfg == nil {
		return fmt.Sprintf("%s=%s", valueName, tag), nil
	}

	ref, err := docker.ParseReference(tag)
	if err != nil {
		return "", errors.Wrapf(err, "cannot parse the image reference %s", tag)
	}

	var imageTag string
	if ref.Digest != "" {
		imageTag = fmt.Sprintf("%s@%s", ref.Tag, ref.Digest)
	} else {
		imageTag = ref.Tag
	}

	if cfg.ExplicitRegistry {
		if ref.Domain == "" {
			return "", errors.New(fmt.Sprintf("image reference %s has no domain", tag))
		}
		return fmt.Sprintf("%[1]s.registry=%[2]s,%[1]s.repository=%[3]s,%[1]s.tag=%[4]s", valueName, ref.Domain, ref.Path, imageTag), nil
	}

	return fmt.Sprintf("%[1]s.repository=%[2]s,%[1]s.tag=%[3]s", valueName, ref.BaseName, imageTag), nil
}

// pairParamsToArtifacts associates parameters to the build artifact it creates
func pairParamsToArtifacts(builds []build.Artifact, params map[string]string) (map[string]build.Artifact, error) {
	imageToBuildResult := map[string]build.Artifact{}
	for _, b := range builds {
		imageToBuildResult[b.ImageName] = b
	}

	paramToBuildResult := map[string]build.Artifact{}

	for param, imageName := range params {
		b, ok := imageToBuildResult[imageName]
		if !ok {
			return nil, fmt.Errorf("no build present for %s", imageName)
		}

		paramToBuildResult[param] = b
	}

	return paramToBuildResult, nil
}

// expand parses and executes template s with an optional environment map
func expand(s string, envMap map[string]string) (string, error) {
	tmpl, err := util.ParseEnvTemplate(s)
	if err != nil {
		return "", errors.Wrap(err, "parsing template")
	}

	return util.ExecuteEnvTemplate(tmpl, envMap)
}
