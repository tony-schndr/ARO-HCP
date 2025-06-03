@description('Specifies a project name that is used to generate the Event Hub name and the Namespace name.')
param projectName string

@description('Specifies the Azure location for all resources.')
param location string = resourceGroup().location

@description('Event Hub Namespace')
param eventHubNamespaceName string

@description('Specifies the messaging tier for Event Hub Namespace.')
@allowed([
  'Basic'
  'Standard'
])

param eventHubSku string = 'Standard'
var eventHubName = projectName

resource eventHubNamespace 'Microsoft.EventHub/namespaces@2021-11-01' = {
  name: eventHubNamespaceName
  location: location
  sku: {
    name: eventHubSku
    tier: eventHubSku
    capacity: 1
  }
  properties: {
    isAutoInflateEnabled: false
    maximumThroughputUnits: 0
  }
}

resource eventHub 'Microsoft.EventHub/namespaces/eventhubs@2021-11-01' = {
  parent: eventHubNamespace
  name: eventHubName
  properties: {
    messageRetentionInDays: 7
    partitionCount: 1
  }
}

resource storage 'Microsoft.Storage/storageAccounts@2024-01-01' = {
  kind: 'StorageV2'
  location: resourceGroup().location
  name: 'adacheckpoint'
  sku: {
    name: 'Standard_LRS'
  }
}

resource blobContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2024-01-01' = {
  name: '${storage.name}/default/checkpoints'
  properties: {
    publicAccess: 'None' // or omit this entirely for default private
  }
}

output eventHubNamespaceId string = eventHubNamespace.id
output eventHubId string = eventHub.id
output storageConnectionString string = listKeys(storage.id, storage.apiVersion).keys[0].value
output blobContainerName string = blobContainer.name
