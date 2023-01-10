#!/bin/bash

##############################################################################
# install-scale-mesh-demo.sh
#
# Installs the kiali music store demo application
# https://github.com/kiali/demos/tree/master/scale-mesh
# Works on both openshift and non-openshift environments.
##############################################################################

: ${CLIENT_EXE:=oc}
: ${DELETE_DEMOS:=false}
: ${SMESH:=scale-mesh}
: ${BASE_URL:=https://raw.githubusercontent.com/kiali/demos/master}
: ${NUM_NS:=1}
: ${NUM_WORKLOADS:=50}

apply_network_attachment() {
  NAME=$1
cat <<NAD | $CLIENT_EXE -n ${NAME} apply -f -
apiVersion: "k8s.cni.cncf.io/v1"
kind: NetworkAttachmentDefinition
metadata:
  name: istio-cni
NAD
    cat <<SCC | $CLIENT_EXE apply -f -
apiVersion: security.openshift.io/v1
kind: SecurityContextConstraints
metadata:
  name: ${NAME}-scc
runAsUser:
  type: RunAsAny
seLinuxContext:
  type: RunAsAny
supplementalGroups:
  type: RunAsAny
users:
- "system:serviceaccount:${NAME}:default"
- "system:serviceaccount:${NAME}:${NAME}"
SCC
}

install_scale_mesh_demo() {
  x=0
  while [ $x -lt ${NUM_NS} ]
  do
    if [ "${IS_OPENSHIFT}" == "true" ]; then
      ${CLIENT_EXE} new-project depth-${x}
      apply_network_attachment depth-${x}
    else
      ${CLIENT_EXE} create ns depth-${x}
    fi
    ${CLIENT_EXE} label namespace  depth-${x} istio-injection=enabled --overwrite=true
    x=$(( $x + 1 ))
  done

  bash <(curl -L ${BASE_URL}/scale-mesh/scale-mesh.sh) install --mesh-type depth --versions 3 --apps ${NUM_WORKLOADS} --services ${NUM_WORKLOADS} -tgr 50
}

while [ $# -gt 0 ]; do
  key="$1"
  case $key in
    -c|--client)
      CLIENT_EXE="$2"
      shift;shift
      ;;
    -n|--namespaces)
      NUM_NS="$2"
      shift;shift
      ;;
    -wk|--workloads)
      NUM_WORKLOADS="$2"
      shift;shift
      ;;
    -d|--delete)
      DELETE_DEMOS="$2"
      shift;shift
      ;;
    -h|--help)
      cat <<HELPMSG
Valid command line arguments:
  -c|--client: either 'oc' or 'kubectl'
  -n|--namespaces: Number of namespaces. Default: 1
  -d|--delete: if 'true' demos will be deleted; otherwise, they will be installed
  -wk|--workloads: Number of workloads. Default: 50
  -h|--help: this text
HELPMSG
      exit 1
      ;;
    *)
      echo "Unknown argument [$key]. Aborting."
      exit 1
      ;;
  esac
done

IS_OPENSHIFT="false"
if [[ "${CLIENT_EXE}" = *"oc" ]]; then
  IS_OPENSHIFT="true"
fi

echo "CLIENT_EXE=${CLIENT_EXE}"
echo "IS_OPENSHIFT=${IS_OPENSHIFT}"

if [ "${DELETE_DEMOS}" != "true" ]; then
  echo "Installing the ${SMESH} app in the ${SMESH} namespace..."
  install_scale_mesh_demo
else
  echo "Deleting the '${SMESH}' app in the '${SMESH}' namespace..."

  bash <(curl -L ${BASE_URL}/scale-mesh/scale-mesh.sh) uninstall --mesh-type depth --versions 3

  x=0
  while [ $x -lt ${NUM_NS} ]
  do
    if [ "${IS_OPENSHIFT}" == "true" ]; then
      ${CLIENT_EXE} delete project depth-${x}
      ${CLIENT_EXE} delete SecurityContextConstraints depth-${x}-scc
    else
      ${CLIENT_EXE} delete ns depth-${x} --ignore-not-found=true
    fi

    x=$(( $x + 1 ))
  done
fi