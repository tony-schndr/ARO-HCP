using '../templates/hypershift-lookup.bicep'

param aksClusterName = '{{ .mgmt.aks.name }}'
param hypershiftMsiName = 'hypershift'
param etcdBackupJobMsiName = 'etcd-backup-job'

