param eventHubNamespaceName string
param eventHubName string
param principalId string

resource eventHubNamespace 'Microsoft.EventHub/namespaces@2021-11-01' existing = {
  name: eventHubNamespaceName
}

resource eventHub 'Microsoft.EventHub/namespaces/eventhubs@2021-11-01' existing = {
  parent: eventHubNamespace
  name : eventHubName
}

resource eventHubReceiverAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: eventHub 
  name: guid(eventHub.name, principalId)
  properties: {
    roleDefinitionId:  subscriptionResourceId(
    'Microsoft.Authorization/roleDefinitions/',
    'a638d3c7-ab3a-418d-83e6-5f17a39d4fde'
  )
    principalId: principalId 
    principalType: 'ServicePrincipal'
  }
}

resource storage 'Microsoft.Storage/storageAccounts@2024-01-01' existing = {
  name: 'adacheckpoint'
}

resource storageAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: storage 
  name: guid(storage.name, principalId)
  properties: {
    roleDefinitionId:  subscriptionResourceId(
    'Microsoft.Authorization/roleDefinitions/',
    'ba92f5b4-2d11-453d-a403-e96b0029c9fe'
  )
    principalId: principalId 
    principalType: 'ServicePrincipal'
  }
}
