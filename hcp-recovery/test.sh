#!/bin/bash

kubectl apply -f - <<EOF
apiVersion: hcprecovery.aro-hcp.azure.com/v1alpha1
kind: HCPRecovery
metadata:
  name: test-recovery-1
  namespace: hcp-recovery
spec:
  hostedClusterName: recovery-xxkd7k
  controlPlaneNamespace: ocm-arohcppers-2tsl3h2gij8kjj45ngl8o3rhol2n059u-recovery-xxkd7k
  hcpClusterAzureResourceId: /subscriptions/1d3378d3-5a3f-4712-85a1-2485495dfc4b/resourceGroups/hcp-recovery-z5z8df4xcqw7/providers/Microsoft.RedHatOpenshift/hcpOpenShiftClusters/recovery-xxkd7k
  hostedClusterNamespace: ocm-arohcppers-2tsl3h2gij8kjj45ngl8o3rhol2n059u
  veleroBackupName: backup-4f0737d1-533e-4723-b976-8ffb00195acf
EOF
