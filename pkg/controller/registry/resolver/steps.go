package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/operator-framework/operator-registry/pkg/api"
	extScheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/cache"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/projection"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/ownerutil"
)

const (
	secretKind            = "Secret"
	BundleSecretKind      = "BundleSecret"
	optionalManifestsProp = "olm.manifests.optional"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(k8sscheme.AddToScheme(scheme))
	utilruntime.Must(extScheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// NewStepResourceForObject returns a new StepResource for the provided object
func NewStepResourceFromObject(obj runtime.Object, catalogSourceName, catalogSourceNamespace string) (v1alpha1.StepResource, error) {
	var resource v1alpha1.StepResource

	// set up object serializer
	serializer := k8sjson.NewSerializer(k8sjson.DefaultMetaFactory, scheme, scheme, false)

	// create an object manifest
	var manifest bytes.Buffer
	err := serializer.Encode(obj, &manifest)
	if err != nil {
		return resource, err
	}

	if err := ownerutil.InferGroupVersionKind(obj); err != nil {
		return resource, err
	}

	gvk := obj.GetObjectKind().GroupVersionKind()

	metaObj, ok := obj.(metav1.Object)
	if !ok {
		return resource, fmt.Errorf("couldn't get object metadata")
	}

	name := metaObj.GetName()
	if name == "" {
		name = metaObj.GetGenerateName()
	}

	// create the resource
	resource = v1alpha1.StepResource{
		Name:                   name,
		Kind:                   gvk.Kind,
		Group:                  gvk.Group,
		Version:                gvk.Version,
		Manifest:               manifest.String(),
		CatalogSource:          catalogSourceName,
		CatalogSourceNamespace: catalogSourceNamespace,
	}

	// BundleSecret is a synthetic kind that OLM uses to distinguish between secrets included in the bundle and
	// pull secrets included in the installplan
	if obj.GetObjectKind().GroupVersionKind().Kind == secretKind {
		resource.Kind = BundleSecretKind
	}

	return resource, nil
}

func NewSubscriptionStepResource(namespace string, info cache.OperatorSourceInfo) (v1alpha1.StepResource, error) {
	return NewStepResourceFromObject(&v1alpha1.Subscription{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      strings.Join([]string{info.Package, info.Channel, info.Catalog.Name, info.Catalog.Namespace}, "-"),
		},
		Spec: &v1alpha1.SubscriptionSpec{
			CatalogSource:          info.Catalog.Name,
			CatalogSourceNamespace: info.Catalog.Namespace,
			Package:                info.Package,
			Channel:                info.Channel,
			StartingCSV:            info.StartingCSV,
			InstallPlanApproval:    v1alpha1.ApprovalAutomatic,
		},
	}, info.Catalog.Name, info.Catalog.Namespace)
}

func V1alpha1CSVFromBundle(bundle *api.Bundle) (*v1alpha1.ClusterServiceVersion, error) {
	csv := &v1alpha1.ClusterServiceVersion{}
	if err := json.Unmarshal([]byte(bundle.CsvJson), csv); err != nil {
		return nil, err
	}
	return csv, nil
}

// NewStepResourceFromBundle returns StepResources and related Namespaces indexed in the same order.
// StepResources don't contain the resource namespace, which is required to uniquely identify a resource.
func NewStepResourceFromBundle(bundle *api.Bundle, namespace, replaces, catalogSourceName, catalogSourceNamespace string) ([]v1alpha1.StepResource, []string, error) {
	csv, err := V1alpha1CSVFromBundle(bundle)
	if err != nil {
		return nil, nil, err
	}

	csv.SetNamespace(namespace)
	csv.Spec.Replaces = replaces
	anno, err := projection.PropertiesAnnotationFromPropertyList(bundle.Properties)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to construct properties annotation for %q: %w", csv.GetName(), err)
	}

	annos := csv.GetAnnotations()
	if annos == nil {
		annos = make(map[string]string)
	}
	annos[projection.PropertiesAnnotationKey] = anno
	csv.SetAnnotations(annos)

	csvStep, err := NewStepResourceFromObject(csv, catalogSourceName, catalogSourceNamespace)
	if err != nil {
		return nil, nil, err
	}
	steps := []v1alpha1.StepResource{}
	namespaces := []string{}

	for _, object := range bundle.Object {
		dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(object), 10)
		unst := &unstructured.Unstructured{}
		if err := dec.Decode(unst); err != nil {
			return nil, nil, err
		}
		namespaces = append(namespaces, unst.GetNamespace())
		if unst.GetObjectKind().GroupVersionKind().Kind == v1alpha1.ClusterServiceVersionKind {
			// Adding the CSV step only here, although it was computed earlier, to keep the namespace and step orders aligned.
			steps = append(steps, csvStep)
			continue
		}

		step, err := NewStepResourceFromObject(unst, catalogSourceName, catalogSourceNamespace)
		if err != nil {
			return nil, nil, err
		}
		steps = append(steps, step)
	}

	operatorServiceAccountSteps, err := NewServiceAccountStepResources(csv, catalogSourceName, catalogSourceNamespace)
	if err != nil {
		return nil, nil, err
	}
	steps = append(steps, operatorServiceAccountSteps...)
	saNamespaces := []string{}
	// Namespace for service accounts step is always catalogSourceNamespace
	for i := 0; i < len(operatorServiceAccountSteps); i++ {
		saNamespaces = append(saNamespaces, catalogSourceNamespace)
	}
	namespaces = append(namespaces, saNamespaces...)

	return steps, namespaces, nil
}

func NewStepsFromBundle(bundle *api.Bundle, namespace, replaces, catalogSourceName, catalogSourceNamespace string) ([]*v1alpha1.Step, error) {
	bundleSteps, _, err := NewStepResourceFromBundle(bundle, namespace, replaces, catalogSourceName, catalogSourceNamespace)
	if err != nil {
		return nil, err
	}

	var steps []*v1alpha1.Step
	for _, s := range bundleSteps {
		steps = append(steps, &v1alpha1.Step{
			Resolving: bundle.CsvName,
			Resource:  s,
			Status:    v1alpha1.StepStatusUnknown,
		})
	}

	return steps, nil
}

// NewQualifiedStepsFromBundle returns the steps to be populated into the InstallPlan for a bundle
// Qualified means that steps may have been flaged as optional.
func NewQualifiedStepsFromBundle(bundle *api.Bundle, namespace, replaces, catalogSourceName, catalogSourceNamespace string,
	logger *logrus.Logger) ([]*v1alpha1.Step, error) {
	bundleSteps, namespaces, err := NewStepResourceFromBundle(bundle, namespace, replaces, catalogSourceName, catalogSourceNamespace)
	if err != nil {
		return nil, err
	}
	var steps []*v1alpha1.Step
	isOptFunc := isOptional(bundle.Properties, logger)
	for i, s := range bundleSteps {
		// Optional manifests are identified by:  group, kind, namespace (optional), name
		// bundleSteps and namespaces share the same index (weak)
		var key manifestKey
		if namespaces[i] == "" {
			key = manifestKey{
				Group: s.Group,
				Kind:  s.Kind,
				Name:  s.Name,
			}
		} else {
			key = manifestKey{
				Group:     s.Group,
				Kind:      s.Kind,
				Namespace: namespaces[i],
				Name:      s.Name,
			}
		}
		optional := isOptFunc(key)
		logger.Debugf("key %s is optional: %t", key, optional)

		// CSV should be positioned first
		if s.Kind == v1alpha1.ClusterServiceVersionKind {
			steps = append([]*v1alpha1.Step{{
				Resolving: bundle.CsvName,
				Resource:  s,
				Optional:  optional,
				Status:    v1alpha1.StepStatusUnknown,
			}}, steps...)
		} else {
			steps = append(steps, &v1alpha1.Step{
				Resolving: bundle.CsvName,
				Resource:  s,
				Optional:  optional,
				Status:    v1alpha1.StepStatusUnknown,
			})
		}
	}

	return steps, nil
}

// NewServiceAccountStepResources returns a list of step resources required to satisfy the RBAC requirements of the given CSV's InstallStrategy
func NewServiceAccountStepResources(csv *v1alpha1.ClusterServiceVersion, catalogSourceName, catalogSourceNamespace string) ([]v1alpha1.StepResource, error) {
	var rbacSteps []v1alpha1.StepResource

	operatorPermissions, err := RBACForClusterServiceVersion(csv)
	if err != nil {
		return nil, err
	}

	for _, perms := range operatorPermissions {
		if perms.ServiceAccount.Name != "default" {
			step, err := NewStepResourceFromObject(perms.ServiceAccount, catalogSourceName, catalogSourceNamespace)
			if err != nil {
				return nil, err
			}
			rbacSteps = append(rbacSteps, step)
		}
		for _, role := range perms.Roles {
			step, err := NewStepResourceFromObject(role, catalogSourceName, catalogSourceNamespace)
			if err != nil {
				return nil, err
			}
			rbacSteps = append(rbacSteps, step)
		}
		for _, roleBinding := range perms.RoleBindings {
			step, err := NewStepResourceFromObject(roleBinding, catalogSourceName, catalogSourceNamespace)
			if err != nil {
				return nil, err
			}
			rbacSteps = append(rbacSteps, step)
		}
		for _, clusterRole := range perms.ClusterRoles {
			step, err := NewStepResourceFromObject(clusterRole, catalogSourceName, catalogSourceNamespace)
			if err != nil {
				return nil, err
			}
			rbacSteps = append(rbacSteps, step)
		}
		for _, clusterRoleBinding := range perms.ClusterRoleBindings {
			step, err := NewStepResourceFromObject(clusterRoleBinding, catalogSourceName, catalogSourceNamespace)
			if err != nil {
				return nil, err
			}
			rbacSteps = append(rbacSteps, step)
		}
	}
	return rbacSteps, nil
}

type optionalManifests struct {
	Manifests []manifestKey `json:"manifests"`
}

type manifestKey struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// isOptional returns a func giving whether a resource is in the list of optional manifests
// or false if any issue occurs.
func isOptional(properties []*api.Property, logger *logrus.Logger) func(key manifestKey) bool {
	if optionals, ok := getPropertyValue(properties, optionalManifestsProp); ok {
		var optManifests optionalManifests
		if err := json.Unmarshal([]byte(optionals), &optManifests); err == nil {
			return func(key manifestKey) bool {
				for _, optKey := range optManifests.Manifests {
					if optKey == key {
						return true
					}
				}
				return false
			}
		} else {
			logger.WithFields(logrus.Fields{
				optionalManifestsProp: optionals,
				"error":               err,
			}).Warn("unmarshalling error")
		}
	}
	return func(key manifestKey) bool {
		return false
	}
}

// getPropertyValue returns the value of a specific property from an array of Property
// and true if the value is found, false otherwise
func getPropertyValue(properties []*api.Property, propertyType string) (string, bool) {
	for _, property := range properties {
		if property.Type == propertyType {
			return property.Value, true
		}
	}
	return "", false
}
