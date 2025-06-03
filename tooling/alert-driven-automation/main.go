package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2/checkpoints"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

func main() {
	defaultAzureCred, err := azidentity.NewDefaultAzureCredential(nil)

	if err != nil {
		panic(err)
	}

	eventHubUrl := os.Getenv("EVENT_HUB_URL")
	eventHubName := os.Getenv("EVENT_HUB_NAME")
	containerName := "checkpoints"
	// TODO: automate creation of identities
	storageUrl := fmt.Sprintf("https://adacheckpoint.blob.core.windows.net/%s/", containerName)
	if eventHubUrl == "" || eventHubName == "" {
		panic("Missing EVENT_HUB_URL or EVENT_HUB_NAME")
	}
	fmt.Println("connecting to ", eventHubUrl, "hub", eventHubName)
	// create a container client using a connection string and container name
	checkClient, err := container.NewClient(storageUrl, defaultAzureCred, nil)

	if err != nil {
		panic(err)
	}

	// create a checkpoint store that will be used by the event hub
	checkpointStore, err := checkpoints.NewBlobStore(checkClient, &checkpoints.BlobStoreOptions{})

	if err != nil {
		panic(err)
	}

	// create a consumer client using a connection string to the namespace and the event hub

	consumerClient, err := azeventhubs.NewConsumerClient(eventHubUrl, eventHubName, azeventhubs.DefaultConsumerGroup, defaultAzureCred, nil)
	if err != nil {
		panic(err)
	}

	defer consumerClient.Close(context.TODO())

	// create a processor to receive and process events
	processor, err := azeventhubs.NewProcessor(consumerClient, checkpointStore, nil)

	if err != nil {
		panic(err)
	}

	//  for each partition in the event hub, create a partition client with processEvents as the function to process events
	dispatchPartitionClients := func() {
		for {
			partitionClient := processor.NextPartitionClient(context.TODO())

			if partitionClient == nil {
				break
			}

			go func() {
				if err := processEvents(partitionClient); err != nil {
					panic(err)
				}
			}()
		}
	}

	// run all partition clients
	go dispatchPartitionClients()

	processorCtx, processorCancel := context.WithCancel(context.TODO())
	defer processorCancel()

	if err := processor.Run(processorCtx); err != nil {
		panic(err)
	}
}

func processEvents(partitionClient *azeventhubs.ProcessorPartitionClient) error {
	defer closePartitionResources(partitionClient)
	for {
		receiveCtx, receiveCtxCancel := context.WithTimeout(context.TODO(), time.Minute)
		events, err := partitionClient.ReceiveEvents(receiveCtx, 100, nil)
		receiveCtxCancel()

		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		fmt.Printf("Processing %d event(s)\n", len(events))

		for _, event := range events {
			fmt.Printf("Event received with body %v\n", string(event.Body))
		}

		if len(events) != 0 {
			if err := partitionClient.UpdateCheckpoint(context.TODO(), events[len(events)-1], nil); err != nil {
				return err
			}
		}
	}
}

func closePartitionResources(partitionClient *azeventhubs.ProcessorPartitionClient) {
	defer partitionClient.Close(context.TODO())
}
