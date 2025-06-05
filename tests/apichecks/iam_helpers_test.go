
package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/k8s-config-connector/pkg/gvks/supportedgvks"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// KCCResource holds simplified information about a KCC Custom Resource Definition.
type KCCResource struct {
	Kind     string
	IsDirect bool
}

// ProtoService holds the name of a Protobuf service and its methods.
type ProtoService struct {
	Name    string
	Methods []string
}

// findDirectResources identifies resources that are purely direct,
// excluding those generated from TF or DCL.
func findDirectResources(t *testing.T) map[string]KCCResource {
	resources := make(map[string]KCCResource)
	for gvk := range supportedgvks.SupportedGVKs {
		if supportedgvks.IsDirectByGVK(gvk) {
			resources[gvk.Kind] = KCCResource{
				Kind:     gvk.Kind,
				IsDirect: true,
			}
		}
	}
	return resources
}

// findProtoServicesFromPB reads a binary .pb file and returns a map of services.
func findProtoServicesFromPB(pbPath string) (map[string]ProtoService, error) {
	services := make(map[string]ProtoService)
	pbBytes, err := os.ReadFile(pbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read .pb file %q: %w", pbPath, err)
	}

	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(pbBytes, &fds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal FileDescriptorSet: %w", err)
	}

	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, fmt.Errorf("failed to convert FileDescriptorSet to files: %w", err)
	}

	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		serviceDescriptors := fd.Services()
		for i := 0; i < serviceDescriptors.Len(); i++ {
			serviceDesc := serviceDescriptors.Get(i)
			serviceName := string(serviceDesc.Name())
			var methods []string
			methodDescriptors := serviceDesc.Methods()
			for j := 0; j < methodDescriptors.Len(); j++ {
				methodDesc := methodDescriptors.Get(j)
				methods = append(methods, string(methodDesc.Name()))
			}
			services[serviceName] = ProtoService{
				Name:    serviceName,
				Methods: methods,
			}
		}
		return true
	})

	return services, nil
}

// mapKindsToImplementationFiles walks the direct controller directories, parses Go files,
// and looks for `registry.RegisterModel` calls to map a KCC Kind to its implementation file.
func mapKindsToImplementationFiles(t *testing.T, directControllerDir string) (map[string]string, error) {
	kindToFile := make(map[string]string)
	fset := token.NewFileSet()

	err := filepath.Walk(directControllerDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}

		node, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}

		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "RegisterModel" {
				if len(call.Args) > 0 {
					// Argument is likely `krm.SomeKindGVK`
					if gvkExpr, ok := call.Args[0].(*ast.SelectorExpr); ok {
						kind := strings.TrimSuffix(gvkExpr.Sel.Name, "GVK")
						kindToFile[kind] = path
					}
				}
			}
			return true
		})
		return nil
	})

	return kindToFile, err
}

// goFileImplementsIAM parses a single Go source file and checks for the
// presence of GetIAMPolicy and SetIAMPolicy methods.
func goFileImplementsIAM(filePath string) (hasGet bool, hasSet bool, err error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		return false, false, err
	}

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		// We only care about methods, which have a receiver.
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}

		switch fn.Name.Name {
		case "GetIAMPolicy":
			hasGet = true
		case "SetIAMPolicy":
			hasSet = true
		}
		return true
	})

	return hasGet, hasSet, nil
}
