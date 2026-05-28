@description('The name of the AKS cluster where HyperShift will be deployed')
param aksClusterName string

@description('The name of the HyperShift operator managed identity')
param hypershiftMsiName string

@description('The name of the etcd backup job managed identity')
param etcdBackupJobMsiName string

//
//   H Y P E R S H I F T   L O O K U P
//

resource aksCluster 'Microsoft.ContainerService/managedClusters@2024-02-01' existing = {
  name: aksClusterName
}

output csiSecretStoreClientId string = aksCluster.properties.addonProfiles.azureKeyvaultSecretsProvider.identity.clientId

//
//   W O R K L O A D   I D E N T I T I E S
//

resource etcdBackupJobIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  name: etcdBackupJobMsiName
}

output etcdBackupJobMsiClientId string = etcdBackupJobIdentity.properties.clientId
output tenantId string = tenant().tenantId
