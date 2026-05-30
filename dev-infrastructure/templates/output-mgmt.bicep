@description('Name of the management cluster.')
param mgmtClusterName string

@description('Name of the backup storage account.')
param backupsStorageAccountName string

@description('Number of Velero shards per management cluster')
param veleroShardCount int = 1

resource aksCluster 'Microsoft.ContainerService/managedClusters@2024-10-01' existing = {
  name: mgmtClusterName
}
output azureKeyvaultSecretsProviderIdentityClientId string = aksCluster.properties.addonProfiles.azureKeyvaultSecretsProvider.identity.clientId

output azureAksManagementClusterResourceId string = aksCluster.id

// Why not retrieve the account name from config/config.yaml?
// Because the config could contain account name with an upper case (regionShortName), storage accounts must be lower case.
resource hcpBackupsStorageAccount 'Microsoft.Storage/storageAccounts@2023-01-01' existing = {
  name: backupsStorageAccountName
}

output hcpBackupsStorageAccountName string = hcpBackupsStorageAccount.name

//
//   O A D P   W O R K L O A D   I D E N T I T I E S
//

resource veleroIdentity0 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  scope: resourceGroup()
  name: 'velero-0'
}

resource veleroIdentity1 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = if (veleroShardCount > 1) {
  scope: resourceGroup()
  name: 'velero-1'
}

resource veleroIdentity2 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = if (veleroShardCount > 2) {
  scope: resourceGroup()
  name: 'velero-2'
}

resource veleroIdentity3 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = if (veleroShardCount > 3) {
  scope: resourceGroup()
  name: 'velero-3'
}

// Bicep cannot join() runtime properties in a for-loop (BCP182/BCP138),
// so we build the comma-separated string with ternary concatenation.
output veleroMsiClientIds string = veleroShardCount == 1
  ? veleroIdentity0.properties.clientId
  : veleroShardCount == 2
    ? '${veleroIdentity0.properties.clientId},${veleroIdentity1.properties.clientId}'
    : veleroShardCount == 3
      ? '${veleroIdentity0.properties.clientId},${veleroIdentity1.properties.clientId},${veleroIdentity2.properties.clientId}'
      : '${veleroIdentity0.properties.clientId},${veleroIdentity1.properties.clientId},${veleroIdentity2.properties.clientId},${veleroIdentity3.properties.clientId}'

output tenantId string = tenant().tenantId
output subscriptionId string = subscription().subscriptionId
