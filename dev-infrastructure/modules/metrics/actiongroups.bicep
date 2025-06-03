import { csvToArray } from '../common.bicep'

@description('Comma seperated list of email notifications. Only set in non MSFT environments!')
param devAlertingEmails string

@description('Comma seperated list of action groups for Sev 1 alerts.')
param sev1ActionGroupIDs string

@description('Comma seperated list of action groups for Sev 2 alerts.')
param sev2ActionGroupIDs string

@description('Comma seperated list of action groups for Sev 3 alerts.')
param sev3ActionGroupIDs string

@description('Comma seperated list of action groups for Sev 4 alerts.')
param sev4ActionGroupIDs string

var sev1ActionGroups = csvToArray(sev1ActionGroupIDs)
var sev2ActionGroups = csvToArray(sev2ActionGroupIDs)
var sev3ActionGroups = csvToArray(sev3ActionGroupIDs)
var sev4ActionGroups = csvToArray(sev4ActionGroupIDs)

var emailAdresses = csvToArray(devAlertingEmails)
resource emailActions 'Microsoft.Insights/actionGroups@2023-01-01' = [
  for email in emailAdresses: {
    name: email
    location: 'Global'
    properties: {
      groupShortName: substring(uniqueString(email), 0, 8)
      enabled: true
      emailReceivers: [
        {
          name: split(email, '@')[0]
          emailAddress: email
          useCommonAlertSchema: true
        }
      ]
    }
  }
]


resource kafkaActions 'Microsoft.Insights/actionGroups@2023-01-01' = {
  name: 'kafka-action'
  location: 'Global'
  properties: {
    groupShortName: substring(uniqueString('kafka-action'), 0, 8)
    enabled: true
    eventHubReceivers: [
      {
        name: 'eventhub-receiver'
        eventHubNameSpace: 'aro-hcp-events'
        eventHubName: 'alerts'
        subscriptionId: subscription().id
        useCommonAlertSchema: true
      }
    ]
  }
}

var actionGroupsCreated = [for (j, index) in emailAdresses: emailActions[index].id]
var kafkaActionGroupId = kafkaActions.id

output allSev1ActionGroups array = union(filter(sev1ActionGroups, a => (a != '')), actionGroupsCreated, [kafkaActionGroupId])
output allSev2ActionGroups array = union(filter(sev2ActionGroups, a => (a != '')), actionGroupsCreated, [kafkaActionGroupId])
output allSev3ActionGroups array = union(filter(sev3ActionGroups, a => (a != '')), actionGroupsCreated, [kafkaActionGroupId])
output allSev4ActionGroups array = union(filter(sev4ActionGroups, a => (a != '')), actionGroupsCreated, [kafkaActionGroupId])
