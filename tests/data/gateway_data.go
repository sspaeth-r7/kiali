package data

import (
	api_networking_v1beta1 "istio.io/api/networking/v1beta1"
	networking_v1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
)

func CreateEmptyGateway(name, namespace string, selector map[string]string) *networking_v1beta1.Gateway {
	gw := networking_v1beta1.Gateway{}
	gw.Name = name
	gw.Namespace = namespace
	gw.Kind = "Gateway"
	gw.APIVersion = "networking.istio.io/v1beta1"
	gw.Spec.Selector = selector
	return &gw
}

func AddServerToGateway(server *api_networking_v1beta1.Server, gw *networking_v1beta1.Gateway) *networking_v1beta1.Gateway {
	gw.Spec.Servers = append(gw.Spec.Servers, server)
	return gw
}

func CreateServer(hosts []string, port uint32, portName, protocolName string) *api_networking_v1beta1.Server {
	server := api_networking_v1beta1.Server{
		Hosts: hosts,
		Port:  CreateEmptyPortDefinition(port, portName, protocolName),
	}
	return &server
}

func AddGatewaysToVirtualService(gateways []string, vs *networking_v1beta1.VirtualService) *networking_v1beta1.VirtualService {
	vs.Spec.Gateways = gateways
	return vs
}
