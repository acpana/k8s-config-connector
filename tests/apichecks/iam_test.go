
package lint

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

type ServiceMapping struct {
	Spec struct {
		Resources []struct {
			Kind        string `yaml:"kind"`
			ServiceName string `yaml:"serviceName"`
		} `yaml:"resources"`
	} `yaml:"spec"`
}

func buildKindToServiceMap(t *testing.T, serviceMappingDir string) (map[string]string, error) {
	files, err := ioutil.ReadDir(serviceMappingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read service mapping directory: %w", err)
	}

	kindToService := make(map[string]string)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}

		filePath := filepath.Join(serviceMappingDir, file.Name())
		yamlFile, err := ioutil.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read YAML file %s: %w", filePath, err)
		}

		var mapping ServiceMapping
		err = yaml.Unmarshal(yamlFile, &mapping)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal YAML file %s: %w", filePath, err)
		}

		for _, resource := range mapping.Spec.Resources {
			if resource.ServiceName != "" {
				kindToService[resource.Kind] = resource.ServiceName
			}
		}
	}
	return kindToService, nil
}

func TestHasDirectIAMImplementation(t *testing.T) {
	kccRepoPath := os.Getenv("REPO_ROOT")
	if kccRepoPath == "" {
		t.Fatalf("REPO_ROOT environment variable not set")
	}

	// 1. Find all "true" direct resources.
	directResources := findDirectResources(t)

	// 2. Load all proto services from the .pb file.
	pbFilePath := filepath.Join(kccRepoPath, ".build", "googleapis.pb")
	protoServices, err := findProtoServicesFromPB(pbFilePath)
	if err != nil {
		t.Fatalf("Error processing .pb file: %v", err)
	}

	// 3. Map KCC Kinds to the Go files that implement their models.
	directControllerDir := filepath.Join(kccRepoPath, "pkg", "controller", "direct")
	kindToImplFile, err := mapKindsToImplementationFiles(t, directControllerDir)
	if err != nil {
		t.Fatalf("Error mapping kinds to implementation files: %v", err)
	}

	// 4. Build a map from KCC Kind to Protobuf service name.
	serviceMappingDir := filepath.Join(kccRepoPath, "config", "servicemappings")
	kindToService, err := buildKindToServiceMap(t, serviceMappingDir)
	if err != nil {
		t.Fatalf("Error building kind to service map: %v", err)
	}

	// 5. Check that direct controllers implement IAM methods if they exist in the respective proto.
	var errs []string
	for kind := range directResources {
		serviceName, ok := kindToService[kind]
		if !ok {
			// This is not an error, as many resources do not have a service mapping.
			continue
		}

		service, protoHasService := protoServices[serviceName]
		if !protoHasService {
			errs = append(errs, fmt.Sprintf("proto service '%s' not found for kind %s", serviceName, kind))
			continue
		}

		hasGetIamPolicyProto := false
		hasSetIamPolicyProto := false
		for _, method := range service.Methods {
			if method == "GetIamPolicy" {
				hasGetIamPolicyProto = true
			}
			if method == "SetIamPolicy" {
				hasSetIamPolicyProto = true
			}
		}

		if !hasGetIamPolicyProto && !hasSetIamPolicyProto {
			continue
		}

		implFile, ok := kindToImplFile[kind]
		if !ok {
			errs = append(errs, fmt.Sprintf("direct resource '%s' has IAM in its proto but no Go implementation file was found", kind))
			continue
		}

		hasGetIAMPolicyGo, hasSetIAMPolicyGo, err := goFileImplementsIAM(implFile)
		if err != nil {
			errs = append(errs, fmt.Sprintf("error parsing Go file %s for resource '%s': %v", implFile, kind, err))
			continue
		}

		if hasGetIamPolicyProto && !hasGetIAMPolicyGo {
			errs = append(errs, fmt.Sprintf("direct resource '%s' has 'GetIamPolicy' in its proto but is missing the 'GetIAMPolicy' method in its Go implementation (%s)", kind, implFile))
		}
		if hasSetIamPolicyProto && !hasSetIAMPolicyGo {
			errs = append(errs, fmt.Sprintf("direct resource '%s' has 'SetIamPolicy' in its proto but is missing the 'SetIAMPolicy' method in its Go implementation (%s)", kind, implFile))
		}
	}
}
