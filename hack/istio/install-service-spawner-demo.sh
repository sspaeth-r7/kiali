#!/bin/bash

##############################################################################
# install-service-spawner-demo.sh
#
# Installs the kiali service spawner demo application
# https://github.com/kiali/demos/tree/master/service-spawner
# Works on both openshift and non-openshift environments.
##############################################################################

: ${CLIENT_EXE:=oc}
: ${DELETE_DEMOS:=false}
: ${SSPAWNER:=service-spawner}
: ${BASE_URL:=https://raw.githubusercontent.com/kiali/demos/master}
: ${NUM_SPAWNS:=10}

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

install_service_spawner_demo() {

  if [ "${IS_OPENSHIFT}" == "true" ]; then
    ${CLIENT_EXE} new-project ${SSPAWNER}
  else
    ${CLIENT_EXE} create ns ${SSPAWNER}
  fi
  ${CLIENT_EXE} label namespace ${SSPAWNER} istio-injection=enabled --overwrite=true
  apply_network_attachment ${SSPAWNER}

  for (( c=0; c<$NUM_SPAWNS; c++ ))
  do
    next=$(($c+1))
    if [[ $next -eq NUM_SPAWNS ]]; then
      next=0
    fi
    curl -L ${BASE_URL}/service-spawner/deployment-tpl.yaml -o deployment-tpl.yaml
    cat deployment-tpl.yaml \
          | sed -e "s:this-service:service-$c:g" \
          | sed -e "s:80:8080:g" \
          | sed -e "s:target-service:service-$next\:8080:g" \
          | sed -e "s:this-namespace:$SSPAWNER:g" \
          | sed -e "s:quay.io/jotak/nginx-hello:nginxdemos/nginx-hello:g" \
          > tmp-$c.yaml
    ${CLIENT_EXE} apply -f tmp-${c}.yaml -n ${SSPAWNER}
  done
  rm deployment-tpl.yaml
  for (( c=0; c<$NUM_SPAWNS; c++ ))
  do
      rm tmp-${c}.yaml
  done
}

while [ $# -gt 0 ]; do
  key="$1"
  case $key in
    -c|--client)
      CLIENT_EXE="$2"
      shift;shift
      ;;
    -n|-spawns)
      NUM_SPAWNS="$2"
      shift;shift
      ;;
    -d|-delete)
      DELETE_DEMOS="$2"
      shift;shift
      ;;
    -h|--help)
      cat <<HELPMSG
Valid command line arguments:
  -c|--client: either 'oc' or 'kubectl'
  -n|--spawns: Number of spawns. Default: 10
  -d|--delete: if 'true' demos will be deleted; otherwise, they will be installed
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
  echo "Installing the ${SSPAWNER} app in the ${SSPAWNER} namespace..."
  install_service_spawner_demo
else
  echo "Deleting the '${SSPAWNER}' app in the '${SSPAWNER}' namespace..."

  ${CLIENT_EXE} delete all -l project=service-spawner

  if [ "${IS_OPENSHIFT}" == "true" ]; then
    ${CLIENT_EXE} delete project ${SSPAWNER}
    ${CLIENT_EXE} delete SecurityContextConstraints ${SSPAWNER}-scc
  else
    ${CLIENT_EXE} delete ns ${SSPAWNER} --ignore-not-found=true
  fi
fi