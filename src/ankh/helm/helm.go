package helm

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ankh"
	"ankh/util"

	"gopkg.in/yaml.v2"
)

type chartFiles struct {
	Dir                      string
	ChartDir                 string
	ValuesPath               string
	AnkhValuesPath           string
	AnkhResourceProfilesPath string
}

func FindChartFiles(ctx *ankh.ExecutionContext, name string, version string, ankhFile ankh.AnkhFile) (chartFiles, error) {
	dirPath := filepath.Join(filepath.Dir(ankhFile.Path), "charts", name)
	_, dirErr := os.Stat(dirPath)

	files := chartFiles{}
	// Setup a directory where we'll either copy the chart files, if we've got a
	// directory, or we'll download and extract a tarball to the temp dir. Then
	// we'll mutate some of the ankh specific files based on the current
	// environment and resource profile. Then we'll use those files as arguments
	// to the helm command.
	tmpDir, err := ioutil.TempDir(ctx.DataDir, name+"-")
	if err != nil {
		return files, err
	}

	tarballFileName := fmt.Sprintf("%s-%s.tgz", name, version)
	tarballURL := fmt.Sprintf("%s/%s", strings.TrimRight(
		ctx.AnkhConfig.CurrentContext.HelmRegistryURL, "/"), tarballFileName)

	// If we already have a dir, let's just copy it to a temp directory so we can
	// make changes to the ankh specific yaml files before passing them as `-f`
	// args to `helm template`
	if dirErr == nil {
		if err := util.CopyDir(dirPath, filepath.Join(tmpDir, name)); err != nil {
			return files, err
		}
	} else {

		// TODO: this code should be modified to properly fetch charts
		ok := false
		for attempt := 1; attempt <= 5; attempt++ {
			ctx.Logger.Debugf("downloading chart from %s (attempt %v)", tarballURL, attempt)
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			client := &http.Client{
				Transport: tr,
				Timeout:   time.Duration(5 * time.Second),
			}
			resp, err := client.Get(tarballURL)
			if err != nil {
				return files, err
			}
			defer resp.Body.Close()

			if resp.StatusCode == 200 {
				ctx.Logger.Debugf("untarring chart to %s", tmpDir)
				if err = util.Untar(tmpDir, resp.Body); err != nil {
					return files, err
				}

				ok = true
				break
			} else {
				ctx.Logger.Warningf("got a status code %v when trying to call %s (attempt %v)", resp.StatusCode, tarballURL, attempt)
			}
		}
		if !ok {
			return files, fmt.Errorf("failed to fetch helm chart from URL: %v", tarballURL)
		}
	}

	chartDir := filepath.Join(tmpDir, name)
	files = chartFiles{
		Dir:                      tmpDir,
		ChartDir:                 chartDir,
		ValuesPath:               filepath.Join(chartDir, "values.yaml"),
		AnkhValuesPath:           filepath.Join(chartDir, "ankh-values.yaml"),
		AnkhResourceProfilesPath: filepath.Join(chartDir, "ankh-resource-profiles.yaml"),
	}

	return files, nil
}

func templateChart(ctx *ankh.ExecutionContext, chart ankh.Chart, ankhFile ankh.AnkhFile) (string, error) {
	currentContext := ctx.AnkhConfig.CurrentContext
	helmArgs := []string{"helm", "template", "--kube-context", currentContext.KubeContext, "--namespace", ankhFile.Namespace}

	if currentContext.Release != "" {
		helmArgs = append(helmArgs, []string{"--release", currentContext.Release}...)
	}

	// Check if Global contains anything and append `--set` flags to the helm
	// command for each item
	if currentContext.Global != nil {
		for _, item := range util.Collapse(currentContext.Global, nil, nil) {
			helmArgs = append(helmArgs, "--set", "global."+item)
		}
	}

	files, err := FindChartFiles(ctx, chart.Name, chart.Version, ankhFile)
	if err != nil {
		return "", err
	}

	// Load `values` from chart
	_, valuesErr := os.Stat(files.AnkhValuesPath)
	if valuesErr == nil {
		if _, err := CreateReducedYAMLFile(files.AnkhValuesPath, currentContext.Environment); err != nil {
			return "", fmt.Errorf("unable to process ankh-values.yaml file for chart '%s': %v", chart.Name, err)
		}
		helmArgs = append(helmArgs, "-f", files.AnkhValuesPath)
	}

	// Load `profiles` from chart
	_, resourceProfilesError := os.Stat(files.AnkhResourceProfilesPath)
	if resourceProfilesError == nil {
		if _, err := CreateReducedYAMLFile(files.AnkhResourceProfilesPath, currentContext.ResourceProfile); err != nil {
			return "", fmt.Errorf("unable to process ankh-resource-profiles.yaml file for chart '%s': %v", chart.Name, err)
		}
		helmArgs = append(helmArgs, "-f", files.AnkhResourceProfilesPath)
	}

	// TODO: add validation for secrets
	if chart.Secrets != nil {
		secretsPath := filepath.Join(files.Dir, "secrets.yaml")
		secretsBytes, err := yaml.Marshal(chart.Secrets[currentContext.Environment])
		if err != nil {
			return "", err
		}

		if err := ioutil.WriteFile(secretsPath, secretsBytes, 0644); err != nil {
			return "", err
		}

		helmArgs = append(helmArgs, "-f", secretsPath)
	}

	// Load `default-values` from ankhFile
	if chart.DefaultValues != nil {
		defaultValuesPath := filepath.Join(files.Dir, "default-values.yaml")
		defaultValuesBytes, err := yaml.Marshal(chart.DefaultValues)
		if err != nil {
			return "", err
		}

		if err := ioutil.WriteFile(defaultValuesPath, defaultValuesBytes, 0644); err != nil {
			return "", err
		}

		helmArgs = append(helmArgs, "-f", defaultValuesPath)
	}

	// Load `values` from ankhFile
	if chart.Values != nil && chart.Values[currentContext.Environment] != nil {
		valuesPath := filepath.Join(files.Dir, "values.yaml")
		valuesBytes, err := yaml.Marshal(chart.Values[currentContext.Environment])
		if err != nil {
			return "", err
		}

		if err := ioutil.WriteFile(valuesPath, valuesBytes, 0644); err != nil {
			return "", err
		}

		helmArgs = append(helmArgs, "-f", valuesPath)
	}

	// Load `resource-profiles` from ankhFile
	if chart.ResourceProfiles != nil && chart.ResourceProfiles[currentContext.ResourceProfile] != nil {
		resourceProfilesPath := filepath.Join(files.Dir, "resource-profiles.yaml")
		resourceProfilesBytes, err := yaml.Marshal(chart.ResourceProfiles[currentContext.ResourceProfile])

		if err != nil {
			return "", err
		}

		if err := ioutil.WriteFile(resourceProfilesPath, resourceProfilesBytes, 0644); err != nil {
			return "", err
		}

		helmArgs = append(helmArgs, "-f", resourceProfilesPath)
	}

	helmArgs = append(helmArgs, files.ChartDir)

	ctx.Logger.Debugf("running helm command %s", strings.Join(helmArgs, " "))

	helmCmd := exec.Command(helmArgs[0], helmArgs[1:]...)
	helmOutput, err := helmCmd.CombinedOutput()

	if err != nil {
		return "", fmt.Errorf("error running the helm command: %s", helmOutput)
	}

	return string(helmOutput), nil
}

func CreateReducedYAMLFile(filename, key string) ([]byte, error) {
	in := make(map[string]interface{})
	var result []byte
	inBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return result, err
	}

	if err = yaml.UnmarshalStrict(inBytes, &in); err != nil {
		return result, err
	}

	out := make(map[interface{}]interface{})

	if in[key] == nil {
		return result, fmt.Errorf("missing `%s` key", key)
	}

	switch t := in[key].(type) {
	case map[interface{}]interface{}:
		for k, v := range t {
			// TODO: using `.(string)` here could cause a panic in cases where the
			// key isn't a string, which is pretty uncommon

			// TODO: validate
			out[k.(string)] = v
		}
	default:
		out[key] = in[key]
	}

	outBytes, err := yaml.Marshal(&out)
	if err != nil {
		return result, err
	}

	if err := ioutil.WriteFile(filename, outBytes, 0644); err != nil {
		return result, err
	}

	return outBytes, nil
}

func Template(ctx *ankh.ExecutionContext, ankhFile ankh.AnkhFile) (string, error) {
	finalOutput := ""

	if len(ankhFile.Charts) > 0 {
		ctx.Logger.Debugf("templating charts")

		if ctx.Chart == "" {
			for _, chart := range ankhFile.Charts {
				ctx.Logger.Debugf("templating chart '%s'", chart.Name)
				chartOutput, err := templateChart(ctx, chart, ankhFile)
				if err != nil {
					return finalOutput, err
				}
				finalOutput += chartOutput
			}
		} else {
			for _, chart := range ankhFile.Charts {
				if chart.Name == ctx.Chart {
					ctx.Logger.Debugf("templating chart '%s'", chart.Name)

					chartOutput, err := templateChart(ctx, chart, ankhFile)
					if err != nil {
						return finalOutput, err
					}
					return chartOutput, nil
				}
			}
			ctx.Logger.Fatalf("Chart %s was specified with `--chart` but does not exist in the charts array", ctx.Chart)
		}
	} else {
		ctx.Logger.Infof(
			"%s does not contain any charts. Template commands only operate on ankh.yaml files containing charts",
			ctx.AnkhFilePath)
	}
	return finalOutput, nil
}
