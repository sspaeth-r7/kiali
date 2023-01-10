package references

import (
	"testing"

	"github.com/stretchr/testify/assert"
	networking_v1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	security_v1beta "istio.io/client-go/pkg/apis/security/v1beta1"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tests/data"
)

func prepareTestForPeerAuth(pa *security_v1beta.PeerAuthentication, drs []*networking_v1beta1.DestinationRule) models.IstioReferences {
	drReferences := PeerAuthReferences{
		MTLSDetails: kubernetes.MTLSDetails{
			PeerAuthentications: []*security_v1beta.PeerAuthentication{pa},
			DestinationRules:    drs,
			EnabledAutoMtls:     false,
		},
		WorkloadsPerNamespace: map[string]models.WorkloadList{
			"istio-system": data.CreateWorkloadList("istio-system",
				data.CreateWorkloadListItem("grafana", map[string]string{"app": "grafana"})),
			"bookinfo": data.CreateWorkloadList("bookinfo",
				data.CreateWorkloadListItem("details", map[string]string{"app": "details"})),
		},
	}
	return *drReferences.References()[models.IstioReferenceKey{ObjectType: "peerauthentication", Namespace: pa.Namespace, Name: pa.Name}]
}

func TestMeshPeerAuthDisabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "disable-mesh-mtls", "istio-system"),
		getPADestinationRules(t, "istio-system"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Len(references.ObjectReferences, 1)
	assert.Equal(references.ObjectReferences[0].Name, "disable-mtls")
	assert.Equal(references.ObjectReferences[0].Namespace, "istio-system")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestNamespacePeerAuthDisabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "disable-namespace-mtls", "bookinfo"),
		getPADestinationRules(t, "bookinfo"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Len(references.ObjectReferences, 1)
	assert.Equal(references.ObjectReferences[0].Name, "disable-namespace")
	assert.Equal(references.ObjectReferences[0].Namespace, "bookinfo")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestMeshNamespacePeerAuthDisabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "disable-namespace-mtls", "bookinfo"),
		getPADestinationRules(t, "istio-system"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Equal(references.ObjectReferences[0].Name, "disable-mtls")
	assert.Equal(references.ObjectReferences[0].Namespace, "istio-system")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestMeshPeerAuthEnabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "strict-mesh-mtls", "istio-system"),
		getPADestinationRules(t, "istio-system"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Len(references.ObjectReferences, 1)
	assert.Equal(references.ObjectReferences[0].Name, "enable-mtls")
	assert.Equal(references.ObjectReferences[0].Namespace, "istio-system")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestNamespacePeerAuthEnabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "strict-namespace-mtls", "bookinfo"),
		getPADestinationRules(t, "bookinfo"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Len(references.ObjectReferences, 1)
	assert.Equal(references.ObjectReferences[0].Name, "enable-namespace")
	assert.Equal(references.ObjectReferences[0].Namespace, "bookinfo")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestMeshNamespacePeerAuthEnabledReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "strict-namespace-mtls", "bookinfo"),
		getPADestinationRules(t, "istio-system"))
	assert.Empty(references.ServiceReferences)

	// Check Workload references empty
	assert.Empty(references.WorkloadReferences)

	// Check DR and AuthPolicy references
	assert.Len(references.ObjectReferences, 1)
	assert.Equal(references.ObjectReferences[0].Name, "enable-mtls")
	assert.Equal(references.ObjectReferences[0].Namespace, "istio-system")
	assert.Equal(references.ObjectReferences[0].ObjectType, "destinationrule")
}

func TestMeshPeerAuthWorkloadReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "permissive-mesh-mtls", "istio-system"),
		getPADestinationRules(t, "istio-system"))
	assert.Empty(references.ServiceReferences)
	assert.Empty(references.ObjectReferences)

	// Check Workload references
	assert.Len(references.WorkloadReferences, 1)
	assert.Equal(references.WorkloadReferences[0].Name, "grafana")
	assert.Equal(references.WorkloadReferences[0].Namespace, "istio-system")
}

func TestNamespacePeerAuthWorkloadReferences(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	config.Set(conf)

	// Setup mocks
	references := prepareTestForPeerAuth(getPeerAuth(t, "permissive-namespace-mtls", "bookinfo"),
		getPADestinationRules(t, "bookinfo"))
	assert.Empty(references.ServiceReferences)
	assert.Empty(references.ObjectReferences)

	// Check Workload references
	assert.Len(references.WorkloadReferences, 1)
	assert.Equal(references.WorkloadReferences[0].Name, "details")
	assert.Equal(references.WorkloadReferences[0].Namespace, "bookinfo")
}

func getPADestinationRules(t *testing.T, namespace string) []*networking_v1beta1.DestinationRule {
	loader := yamlFixtureLoader("peer-auth-drs.yaml")
	err := loader.Load()
	if err != nil {
		t.Error("Error loading test data.")
	}

	return loader.FindDestinationRuleIn(namespace)
}

func getPeerAuth(t *testing.T, name, namespace string) *security_v1beta.PeerAuthentication {
	loader := yamlFixtureLoader("peer-auth-drs.yaml")
	err := loader.Load()
	if err != nil {
		t.Error("Error loading test data.")
	}

	return loader.FindPeerAuthentication(name, namespace)
}
