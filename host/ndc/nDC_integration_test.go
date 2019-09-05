// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ndc

import (
	"context"
	"flag"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"

	"github.com/uber/cadence/.gen/go/history"
	"github.com/uber/cadence/.gen/go/shared"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/loggerimpl"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/persistence"
	test "github.com/uber/cadence/common/testing"
	"github.com/uber/cadence/environment"
	"github.com/uber/cadence/host"
)

type (
	nDCIntegrationTestSuite struct {
		// override suite.Suite.Assertions with require.Assertions; this means that s.NotNil(nil) will stop the test,
		// not merely log an error
		*require.Assertions
		suite.Suite
		active    *host.TestCluster
		passive   *host.TestCluster
		logger    log.Logger
		generator test.Generator
	}
)

var (
	clusterName              = []string{"active", "standby"}
	clusterReplicationConfig = []*workflow.ClusterReplicationConfiguration{
		{
			ClusterName: common.StringPtr(clusterName[0]),
		},
		{
			ClusterName: common.StringPtr(clusterName[1]),
		},
	}
)

func TestNDCIntegrationTestSuite(t *testing.T) {

	flag.Parse()
	suite.Run(t, new(nDCIntegrationTestSuite))
}

func (s *nDCIntegrationTestSuite) SetupSuite() {
	zapLogger, err := zap.NewDevelopment()
	// cannot use s.Nil since it is not initialized
	s.Require().NoError(err)
	s.logger = loggerimpl.NewLogger(zapLogger)

	fileName := "../testdata/xdc_integration_test_clusters.yaml"
	if host.TestFlags.TestClusterConfigFile != "" {
		fileName = host.TestFlags.TestClusterConfigFile
	}
	environment.SetupEnv()

	confContent, err := ioutil.ReadFile(fileName)
	s.Require().NoError(err)
	confContent = []byte(os.ExpandEnv(string(confContent)))

	var clusterConfigs []*host.TestClusterConfig
	s.Require().NoError(yaml.Unmarshal(confContent, &clusterConfigs))

	c, err := host.NewCluster(clusterConfigs[0], s.logger.WithTags(tag.ClusterName(clusterName[0])))
	s.Require().NoError(err)
	s.active = c

	c, err = host.NewCluster(clusterConfigs[1], s.logger.WithTags(tag.ClusterName(clusterName[1])))
	s.Require().NoError(err)
	s.passive = c
}

func (s *nDCIntegrationTestSuite) SetupTest() {
	// Have to define our overridden assertions in the test setup. If we did it earlier, s.T() will return nil
	s.Assertions = require.New(s.T())
}

func (s *nDCIntegrationTestSuite) TearDownSuite() {
	s.active.TearDownCluster()
	s.passive.TearDownCluster()
}

func (s *nDCIntegrationTestSuite) TestSimpleNDC() {

	domainName := "test-simple-workflow-ndc-" + common.GenerateRandomString(5)
	client1 := s.active.GetFrontendClient() // active
	regReq := &shared.RegisterDomainRequest{
		Name:                                   common.StringPtr(domainName),
		IsGlobalDomain:                         common.BoolPtr(true),
		Clusters:                               clusterReplicationConfig,
		ActiveClusterName:                      common.StringPtr(clusterName[0]),
		WorkflowExecutionRetentionPeriodInDays: common.Int32Ptr(1),
	}
	err := client1.RegisterDomain(createContext(), regReq)
	s.NoError(err)

	descReq := &shared.DescribeDomainRequest{
		Name: common.StringPtr(domainName),
	}
	resp, err := client1.DescribeDomain(createContext(), descReq)
	s.NoError(err)
	s.NotNil(resp)
	// Wait for domain cache to pick the change
	time.Sleep(cache.DomainCacheRefreshInterval)
	root := &test.NDCTestBranch{
		Batches: make([]test.NDCTestBatch, 0),
	}
	wid := uuid.New()
	rid := uuid.New()
	wt := "event-generator-workflow-type"
	tl := "event-generator-taskList"
	domain := resp.DomainInfo.GetName()
	version := int64(100)

	s.generator = test.InitializeHistoryEventGenerator(domain)
	for s.generator.HasNextVertex() {
		events := s.generator.GetNextVertices()
		newBatch := test.NDCTestBatch{
			Events: events,
		}
		root.Batches = append(root.Batches, newBatch)
	}

	historyClient := s.passive.GetHistoryClient()
	replicationInfo := make(map[string]*shared.ReplicationInfo)
	replicationInfo["active"] = &shared.ReplicationInfo{
		Version:     common.Int64Ptr(version),
		LastEventId: common.Int64Ptr(0),
	}

	replicationInfo["standby"] = &shared.ReplicationInfo{
		Version:     common.Int64Ptr(version),
		LastEventId: common.Int64Ptr(0),
	}

	for _, batch := range root.Batches {
		// Generate a new run history only when the last event is continue as new
		newRunHistory := generateNewRunHistory(batch.Events[len(batch.Events)-1].GetData().(*shared.HistoryEvent), version, domain, wid, rid, wt, tl)
		batchHistory := shared.History{}
		for _, event := range batch.Events {
			historyEvent := event.GetData().(shared.HistoryEvent)
			batchHistory.Events = append(batchHistory.Events, &historyEvent)
		}
		err = historyClient.ReplicateEvents(createContext(), &history.ReplicateEventsRequest{
			SourceCluster: common.StringPtr("active"),
			DomainUUID:    resp.DomainInfo.UUID,
			WorkflowExecution: &shared.WorkflowExecution{
				WorkflowId: common.StringPtr(wid),
				RunId:      common.StringPtr(rid),
			},
			FirstEventId:            batch.Events[0].GetData().(shared.HistoryEvent).EventId,
			NextEventId:             common.Int64Ptr(*batch.Events[len(batch.Events)-1].GetData().(shared.HistoryEvent).EventId + int64(1)),
			Version:                 common.Int64Ptr(version),
			History:                 &batchHistory,
			NewRunHistory:           newRunHistory,
			ForceBufferEvents:       common.BoolPtr(false),
			EventStoreVersion:       common.Int32Ptr(persistence.EventStoreVersionV2),
			NewRunEventStoreVersion: common.Int32Ptr(persistence.EventStoreVersionV2),
			ResetWorkflow:           common.BoolPtr(false),
		})
		s.Nil(err, "Failed to replicate history event")
	}

	// get replicated history events from passive side
	passiveClient := s.passive.GetFrontendClient()
	replicatedHistory, err := passiveClient.GetWorkflowExecutionHistory(createContext(), &shared.GetWorkflowExecutionHistoryRequest{
		Domain: common.StringPtr(domain),
		Execution: &shared.WorkflowExecution{
			WorkflowId: common.StringPtr(wid),
			RunId:      common.StringPtr(rid),
		},
		MaximumPageSize:        common.Int32Ptr(10000),
		NextPageToken:          nil,
		WaitForNewEvent:        common.BoolPtr(false),
		HistoryEventFilterType: shared.HistoryEventFilterTypeAllEvent.Ptr(),
	})
	s.Nil(err, "Failed to get history event from passive side")

	// compare origin events with replicated events
	batchIndex := 0
	batch := root.Batches[batchIndex].Events
	eventIndex := 0
	for _, event := range replicatedHistory.GetHistory().GetEvents() {
		if eventIndex >= len(batch) {
			batchIndex++
			batch = root.Batches[batchIndex].Events
			eventIndex = 0
		}
		originEvent := batch[eventIndex].GetData().(shared.HistoryEvent)
		eventIndex++
		s.Equal(originEvent.GetEventType().String(), event.GetEventType().String(), "The replicated event and the origin event are not the same")
	}
}

func generateNewRunHistory(
	event *shared.HistoryEvent,
	version int64,
	domain string,
	workflowID string,
	runID string,
	workflowType string,
	taskList string,
) *shared.History {

	if event.GetWorkflowExecutionContinuedAsNewEventAttributes() != nil {
		event.WorkflowExecutionContinuedAsNewEventAttributes.NewExecutionRunId = common.StringPtr(uuid.New())
		return &shared.History{
			Events: []*shared.HistoryEvent{
				{
					EventId:   common.Int64Ptr(1),
					Timestamp: common.Int64Ptr(time.Now().UnixNano()),
					EventType: common.EventTypePtr(shared.EventTypeWorkflowExecutionStarted),
					Version:   common.Int64Ptr(version),
					TaskId:    common.Int64Ptr(1),
					WorkflowExecutionStartedEventAttributes: &shared.WorkflowExecutionStartedEventAttributes{
						WorkflowType:         common.WorkflowTypePtr(shared.WorkflowType{Name: common.StringPtr(workflowType)}),
						ParentWorkflowDomain: common.StringPtr(domain),
						ParentWorkflowExecution: &shared.WorkflowExecution{
							WorkflowId: common.StringPtr(workflowID),
							RunId:      common.StringPtr(runID),
						},
						ParentInitiatedEventId: common.Int64Ptr(event.GetEventId()),
						TaskList: common.TaskListPtr(shared.TaskList{
							Name: common.StringPtr(taskList),
							Kind: common.TaskListKindPtr(shared.TaskListKindNormal),
						}),
						ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(10),
						TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(10),
						ContinuedExecutionRunId:             common.StringPtr(runID),
						Initiator:                           shared.ContinueAsNewInitiatorCronSchedule.Ptr(),
						OriginalExecutionRunId:              common.StringPtr(runID),
						Identity:                            common.StringPtr("NDC-test"),
						FirstExecutionRunId:                 common.StringPtr(runID),
						Attempt:                             common.Int32Ptr(0),
						ExpirationTimestamp:                 common.Int64Ptr(time.Now().Add(time.Minute).UnixNano()),
					},
				},
			},
		}
	}
	return nil
}

func createContext() context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 90*time.Second)
	return ctx
}
