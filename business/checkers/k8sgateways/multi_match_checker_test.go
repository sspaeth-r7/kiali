package k8sgateways

import (
	"testing"

	"github.com/stretchr/testify/assert"
	k8s_networking_v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tests/data"
)

func TestCorrectK8sGateways(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)

	assert := assert.New(t)

	k8sgwObject := data.CreateEmptyK8sGateway("validk8sgateway", "test")

	k8sgws := []*k8s_networking_v1alpha2.Gateway{k8sgwObject}

	vals := MultiMatchChecker{
		K8sGateways: k8sgws,
	}.Check()

	assert.Empty(vals)
	_, ok := vals[models.IstioValidationKey{ObjectType: "k8sgateway", Namespace: "test", Name: "validk8sgateway"}]
	assert.False(ok)
}

func TestDuplicateListenersCheckError(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)

	assert := assert.New(t)

	k8sgwObject := data.AddListenerToK8sGateway(data.CreateListener("test", "host.com", 80, "http"),
		data.CreateEmptyK8sGateway("validk8sgateway", "test"))

	k8sgwObject2 := data.AddListenerToK8sGateway(data.CreateListener("test", "host.com", 80, "http"),
		data.CreateEmptyK8sGateway("validk8sgateway2", "test"))

	k8sgws := []*k8s_networking_v1alpha2.Gateway{k8sgwObject, k8sgwObject2}

	vals := MultiMatchChecker{
		K8sGateways: k8sgws,
	}.Check()

	assert.NotEmpty(vals)
	assert.Equal(2, len(vals))
	validation, ok := vals[models.IstioValidationKey{ObjectType: "k8sgateway", Namespace: "test", Name: "validk8sgateway2"}]
	assert.True(ok)
	assert.NotNil(validation)
	assert.True(validation.Valid)
	assert.Greater(len(validation.Checks), 0)
}

func TestDuplicateListenersCheckOk(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)

	assert := assert.New(t)

	k8sgwObject := data.AddListenerToK8sGateway(data.CreateListener("test", "host.es", 80, "http"),
		data.CreateEmptyK8sGateway("validk8sgateway", "test"))

	k8sgwObject2 := data.AddListenerToK8sGateway(data.CreateListener("test", "host.com", 80, "http"),
		data.CreateEmptyK8sGateway("validk8sgateway2", "test"))

	k8sgws := []*k8s_networking_v1alpha2.Gateway{k8sgwObject, k8sgwObject2}

	vals := MultiMatchChecker{
		K8sGateways: k8sgws,
	}.Check()

	assert.Empty(vals)

}

func TestDuplicateAddresssCheckError(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)

	assert := assert.New(t)

	gwAddress := data.CreateGWAddress("IPAddress", "192.168.0.0")
	k8sgwObject := data.AddGwAddressToK8sGateway(gwAddress,
		data.CreateEmptyK8sGateway("validk8sgateway", "test"))

	k8sgwObject2 := data.AddGwAddressToK8sGateway(gwAddress,
		data.CreateEmptyK8sGateway("validk8sgateway2", "test"))

	k8sgws := []*k8s_networking_v1alpha2.Gateway{k8sgwObject, k8sgwObject2}

	vals := MultiMatchChecker{
		K8sGateways: k8sgws,
	}.Check()

	assert.NotEmpty(vals)
	assert.Equal(2, len(vals))
	validation, ok := vals[models.IstioValidationKey{ObjectType: "k8sgateway", Namespace: "test", Name: "validk8sgateway2"}]
	assert.True(ok)
	assert.NotNil(validation)
	assert.True(validation.Valid)
	assert.Greater(len(validation.Checks), 0)

}

func TestDuplicateAddresssCheckOk(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)

	assert := assert.New(t)

	gwAddress := data.CreateGWAddress("IPAddress", "192.168.0.0")
	k8sgwObject := data.AddGwAddressToK8sGateway(gwAddress,
		data.CreateEmptyK8sGateway("validk8sgateway", "test"))

	gwAddress2 := data.CreateGWAddress("IPAddress", "127.0.0.1")
	k8sgwObject2 := data.AddGwAddressToK8sGateway(gwAddress2,
		data.CreateEmptyK8sGateway("validk8sgateway2", "test"))

	k8sgws := []*k8s_networking_v1alpha2.Gateway{k8sgwObject, k8sgwObject2}

	vals := MultiMatchChecker{
		K8sGateways: k8sgws,
	}.Check()

	assert.Empty(vals)

}
