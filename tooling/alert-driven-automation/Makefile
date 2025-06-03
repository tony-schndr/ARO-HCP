-include ../../setup-env.mk
-include ../../helm-cmd.mk

NAMESPACE = "ada"
# Would be replaced with pipeline.yaml
RESOURCE_GROUP = hcp-underlay-pers-bltsch-svc

deploy:
	@kubectl create namespace ${NAMESPACE} --dry-run=client -o json | kubectl apply -f -
	ADA_WI_IDENTITY_CLIENT_ID=$(shell az deployment group show -n cluster -g ${RESOURCE_GROUP} --query properties.outputs.userAssignedIdentities.value -o json | jq -r '.[] | select(.uamiName=="ada") | .uamiClientID') && \
	EVENT_GRID_URL=$(shell az deployment group show -n cluster -g ${RESOURCE_GROUP} --query properties.outputs.userAssignedIdentities.value -o json | jq -r '.[] | select(.uamiName=="eda") | .uamiClientID') && \
	${HELM_CMD} ada deploy/ \
		--namespace ${NAMESPACE} \
		--set serviceAccount.workloadIdentityClientId=$${ADA_WI_IDENTITY_CLIENT_ID} \
		--set eventGridUrl=aro-hcp-events.servicebus.windows.net \
		--set eventHubName=alerts
.PHONY: deploy

image:
	docker build . --build-arg PLATFORM=linux/amd64 -t quay.io/rh_ee_tschneid/ada:latest