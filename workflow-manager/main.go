// workflow-manager looks for batches to be processed from an input bucket,
// and schedules intake-batch tasks for intake-batch-workers to process those
// batches.
//
// It also looks for batches that have been intake'd, and schedules aggregate
// tasks.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/letsencrypt/prio-server/workflow-manager/batchpath"
	"github.com/letsencrypt/prio-server/workflow-manager/monitor"
	"github.com/letsencrypt/prio-server/workflow-manager/storage"
	"github.com/letsencrypt/prio-server/workflow-manager/task"
	wftime "github.com/letsencrypt/prio-server/workflow-manager/time"
	"github.com/letsencrypt/prio-server/workflow-manager/utils"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/push"
)

// BuildInfo is generated at build time - see the Dockerfile.
var BuildInfo string

var k8sNS = flag.String("k8s-namespace", "", "Kubernetes namespace")
var ingestorLabel = flag.String("ingestor-label", "", "Label of ingestion server")
var isFirst = flag.Bool("is-first", false, "Whether this set of servers is \"first\", aka PHA servers")
var maxAge = flag.String("intake-max-age", "1h", "Max age (in Go duration format) for intake batches to be worth processing.")
var ingestorInput = flag.String("ingestor-input", "", "Bucket for input from ingestor (s3:// or gs://) (Required)")
var ingestorIdentity = flag.String("ingestor-identity", "", "Identity to use with ingestor bucket (Required for S3)")
var ownValidationInput = flag.String("own-validation-input", "", "Bucket for input of validation batches from self (s3:// or gs://) (required)")
var ownValidationIdentity = flag.String("own-validation-identity", "", "Identity to use with own validation bucket (Required for S3)")
var peerValidationInput = flag.String("peer-validation-input", "", "Bucket for input of validation batches from peer (s3:// or gs://) (required)")
var peerValidationIdentity = flag.String("peer-validation-identity", "", "Identity to use with peer validation bucket (Required for S3)")
var aggregationPeriod = flag.String("aggregation-period", "3h", "How much time each aggregation covers")
var gracePeriod = flag.String("grace-period", "1h", "Wait this amount of time after the end of an aggregation timeslice to run the aggregation")
var pushGateway = flag.String("push-gateway", "", "Set this to the gateway to use with prometheus. If left empty, workflow-manager will not use prometheus.")
var dryRun = flag.Bool("dry-run", false, "If set, no operations with side effects will be done.")
var taskQueueKind = flag.String("task-queue-kind", "", "Which task queue kind to use.")
var intakeTasksTopic = flag.String("intake-tasks-topic", "", "Name of the topic to which intake-batch tasks should be published")
var aggregateTasksTopic = flag.String("aggregate-tasks-topic", "", "Name of the topic to which aggregate tasks should be published")
var maxEnqueueWorkers = flag.Int("max-enqueue-workers", 100, "Max number of workers that can be used to enqueue jobs")

// Arguments for gcp-pubsub task queue
var gcpPubSubCreatePubSubTopics = flag.Bool("gcp-pubsub-create-topics", false, "Whether to create the GCP PubSub topics used for intake and aggregation tasks.")
var gcpProjectID = flag.String("gcp-project-id", "", "Name of the GCP project ID being used for PubSub.")

// Arguments for aws-sns task queue
var awsSNSRegion = flag.String("aws-sns-region", "", "AWS region in which to publish to SNS topic")
var awsSNSIdentity = flag.String("aws-sns-identity", "", "AWS IAM ARN of the role to be assumed to publish to SNS topics")

// Define flags and arguments for other task queue implementations here.
// Argument names should be prefixed with the corresponding value of
// task-queue-kind to avoid conflicts.

// monitoring things
var (
	intakesStarted            monitor.GaugeMonitor = &monitor.NoopGauge{}
	intakesSkippedDueToMarker monitor.GaugeMonitor = &monitor.NoopGauge{}

	aggregationsStarted            monitor.GaugeMonitor = &monitor.NoopGauge{}
	aggregationsSkippedDueToMarker monitor.GaugeMonitor = &monitor.NoopGauge{}

	workflowManagerLastSuccess monitor.GaugeMonitor = &monitor.NoopGauge{}
	workflowManagerLastFailure monitor.GaugeMonitor = &monitor.NoopGauge{}
	workflowManagerRuntime     monitor.GaugeMonitor = &monitor.NoopGauge{}
)

func fail(format string, args ...interface{}) {
	workflowManagerLastFailure.SetToCurrentTime()
	log.Fatal().Msgf(format, args...)
}

func prepareLogger() {
	zerolog.LevelFieldName = "severity"
	zerolog.TimestampFieldName = "timestamp"
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
}

func main() {
	prepareLogger()
	startTime := time.Now()
	log.Info().Str("app", os.Args[0]).Str("version", BuildInfo).Str("Args", strings.Join(os.Args[1:], ","))
	flag.Parse()

	if *pushGateway != "" {
		pusher := push.New(*pushGateway, "workflow-manager").
			Gatherer(prometheus.DefaultGatherer).
			Grouping("locality", *k8sNS).
			Grouping("ingestor", *ingestorLabel)
		defer func(){
			err := pusher.Push()
			if err != nil {
				log.Err(err).Msg("error occurred with pushing to prometheus")
			}
		}()

		intakesStarted = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_intake_tasks_scheduled",
			Help: "The number of intake-batch tasks successfully scheduled",
		})
		intakesSkippedDueToMarker = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_intake_tasks_skipped_due_to_marker",
			Help: "The number of intake-batch tasks not scheduled because a task marker was found",
		})

		aggregationsStarted = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_aggregation_tasks_scheduled",
			Help: "The number of aggregate tasks successfully scheduled",
		})
		aggregationsSkippedDueToMarker = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_aggregation_tasks_skipped_due_to_marker",
			Help: "The number of aggregate tasks not scheduled because a task marker was found",
		})

		workflowManagerLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_last_success_seconds",
			Help: "Time of last successful run of workflow-manager in seconds since UNIX epoch",
		})

		workflowManagerLastFailure = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_last_failure_seconds",
			Help: "Time of last failed run of workflow-manager in seconds since UNIX epoch",
		})

		workflowManagerRuntime = promauto.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_manager_runtime_seconds",
			Help: "How long successful workflow-manager runs take",
		})
	}

	ownValidationBucket, err := storage.NewBucket(*ownValidationInput, *ownValidationIdentity, *dryRun)
	if err != nil {
		fail("--own-validation-input: %s", err)
		return
	}
	peerValidationBucket, err := storage.NewBucket(*peerValidationInput, *peerValidationIdentity, *dryRun)
	if err != nil {
		fail("--peer-validation-input: %s", err)
		return
	}
	intakeBucket, err := storage.NewBucket(*ingestorInput, *ingestorIdentity, *dryRun)
	if err != nil {
		fail("--ingestor-input: %s", err)
		return
	}

	maxAgeParsed, err := time.ParseDuration(*maxAge)
	if err != nil {
		fail("--max-age: %s", err)
		return
	}

	gracePeriodParsed, err := time.ParseDuration(*gracePeriod)
	if err != nil {
		fail("--grace-period: %s", err)
		return
	}

	aggregationPeriodParsed, err := time.ParseDuration(*aggregationPeriod)
	if err != nil {
		fail("--aggregation-time-slice: %s", err)
		return
	}

	if *taskQueueKind == "" || *intakeTasksTopic == "" || *aggregateTasksTopic == "" {
		fail("--task-queue-kind, --intake-tasks-topic and --aggregate-tasks-topic are required")
		return
	}

	var intakeTaskEnqueuer task.Enqueuer
	var aggregationTaskEnqueuer task.Enqueuer

	switch *taskQueueKind {
	case "gcp-pubsub":
		if *gcpProjectID == "" {
			fail("--gcp-project-id is required for task-queue-kind=gcp-pubsub")
			return
		}

		if *gcpPubSubCreatePubSubTopics {
			if err := task.CreatePubSubTopic(
				*gcpProjectID,
				*intakeTasksTopic,
			); err != nil {
				fail("creating pubsub topic: %s", err)
				return
			}
			if err := task.CreatePubSubTopic(
				*gcpProjectID,
				*aggregateTasksTopic,
			); err != nil {
				fail("creating pubsub topic: %s", err)
				return
			}
		}

		intakeTaskEnqueuer, err = task.NewGCPPubSubEnqueuer(
			*gcpProjectID,
			*intakeTasksTopic,
			*dryRun,
			int32(*maxEnqueueWorkers),
		)
		if err != nil {
			fail("%s", err)
			return
		}

		aggregationTaskEnqueuer, err = task.NewGCPPubSubEnqueuer(
			*gcpProjectID,
			*aggregateTasksTopic,
			*dryRun,
			int32(*maxEnqueueWorkers),
		)
		if err != nil {
			fail("%s", err)
			return
		}
	case "aws-sns":
		if *awsSNSRegion == "" {
			fail("--aws-sns-region is required for task-queue-kind=aws-sns")
			return
		}

		intakeTaskEnqueuer, err = task.NewAWSSNSEnqueuer(
			*awsSNSRegion,
			*awsSNSIdentity,
			*intakeTasksTopic,
			*dryRun,
		)
		if err != nil {
			fail("%s", err)
			return
		}

		aggregationTaskEnqueuer, err = task.NewAWSSNSEnqueuer(
			*awsSNSRegion,
			*awsSNSIdentity,
			*aggregateTasksTopic,
			*dryRun,
		)
		if err != nil {
			fail("%s", err)
			return
		}
	// To implement a new task queue kind, add a case here. You should
	// initialize intakeTaskEnqueuer and aggregationTaskEnqueuer.
	default:
		fail("unknown task queue kind %s", *taskQueueKind)
		return
	}

	aggregationIDs, err := intakeBucket.ListAggregationIDs()
	if err != nil {
		fail("unable to discover aggregation IDs from ingestion bucket: %q", err)
		return
	}

	for _, aggregationID := range aggregationIDs {
		err = scheduleTasks(scheduleTasksConfig{
			aggregationID:           aggregationID,
			isFirst:                 *isFirst,
			clock:                   wftime.DefaultClock(),
			intakeBucket:            intakeBucket,
			ownValidationBucket:     ownValidationBucket,
			peerValidationBucket:    peerValidationBucket,
			intakeTaskEnqueuer:      intakeTaskEnqueuer,
			aggregationTaskEnqueuer: aggregationTaskEnqueuer,
			maxAge:                  maxAgeParsed,
			aggregationPeriod:       aggregationPeriodParsed,
			gracePeriod:             gracePeriodParsed,
		})
		if err != nil {
			log.Err(err).Str("aggregation ID", aggregationID).Msg("Failed to schedule aggregation tasks")
		}

		workflowManagerLastSuccess.SetToCurrentTime()
		endTime := time.Now()

		workflowManagerRuntime.Set(endTime.Sub(startTime).Seconds())
	}

	log.Info().Msg("done")
}

type scheduleTasksConfig struct {
	aggregationID                                           string
	isFirst                                                 bool
	clock                                                   wftime.Clock
	intakeBucket, ownValidationBucket, peerValidationBucket storage.Bucket
	intakeTaskEnqueuer, aggregationTaskEnqueuer             task.Enqueuer
	maxAge, aggregationPeriod, gracePeriod                  time.Duration
}

// scheduleTasks evaluates bucket contents and Kubernetes cluster state to
// schedule new tasks
func scheduleTasks(config scheduleTasksConfig) error {
	intakeInterval := wftime.Interval{
		Begin: config.clock.Now().Add(-config.maxAge),
		End:   config.clock.Now().Add(24 * time.Hour),
	}

	intakeFiles, err := config.intakeBucket.ListBatchFiles(config.aggregationID, intakeInterval)
	if err != nil {
		return err
	}

	intakeBatches, err := batchpath.ReadyBatches(intakeFiles, "batch")
	if err != nil {
		return err
	}

	// Make a set of the tasks for which we have marker objects for efficient
	// lookup later.
	intakeTaskMarkers, err := config.ownValidationBucket.ListIntakeTaskMarkers(config.aggregationID, intakeInterval)
	if err != nil {
		return err
	}

	intakeTaskMarkersSet := map[string]struct{}{}
	for _, marker := range intakeTaskMarkers {
		intakeTaskMarkersSet[marker] = struct{}{}
	}

	err = enqueueIntakeTasks(
		intakeBatches,
		intakeTaskMarkersSet,
		config.ownValidationBucket,
		config.intakeTaskEnqueuer,
	)
	if err != nil {
		return err
	}

	aggInterval := wftime.AggregationInterval(config.clock, config.aggregationPeriod, config.gracePeriod)

	log.Info().Str("aggregation interval", aggInterval.String()).Msg("looking for batches to aggregate")

	ownValidationFiles, err := config.ownValidationBucket.ListBatchFiles(config.aggregationID, aggInterval)
	if err != nil {
		return err
	}

	ownValidityInfix := fmt.Sprintf("validity_%d", utils.Index(config.isFirst))
	ownValidationBatches, err := batchpath.ReadyBatches(ownValidationFiles, ownValidityInfix)
	if err != nil {
		return err
	}

	log.Info().Int("own validations", len(ownValidationBatches)).Msgf("found %d own validations", len(ownValidationBatches))

	peerValidationFiles, err := config.peerValidationBucket.ListBatchFiles(config.aggregationID, aggInterval)
	if err != nil {
		return err
	}

	peerValidityInfix := fmt.Sprintf("validity_%d", utils.Index(!config.isFirst))
	peerValidationBatches, err := batchpath.ReadyBatches(peerValidationFiles, peerValidityInfix)
	if err != nil {
		return err
	}

	log.Info().Int("peer validations", len(peerValidationBatches)).Msgf("found %d peer validations", len(peerValidationBatches))

	// Take the intersection of the sets of own validations and peer validations
	// to get the list of batches we can aggregate.
	// Go doesn't have sets, so we have to use a map[string]bool. We use the
	// batch ID as the key to the set, because batchPath is not a valid map key
	// type, and using a *batchPath wouldn't give us the lookup semantics we
	// want.
	ownValidationsSet := map[string]bool{}
	for _, ownValidationBatch := range ownValidationBatches {
		ownValidationsSet[ownValidationBatch.ID] = true
	}
	aggregationBatches := batchpath.List{}
	for _, peerValidationBatch := range peerValidationBatches {
		if _, ok := ownValidationsSet[peerValidationBatch.ID]; ok {
			aggregationBatches = append(aggregationBatches, peerValidationBatch)
		}
	}

	aggregationTaskMarkers, err := config.ownValidationBucket.ListAggregateTaskMarkers(config.aggregationID)
	if err != nil {
		return err
	}
	aggregationTaskMarkersSet := map[string]struct{}{}
	for _, marker := range aggregationTaskMarkers {
		aggregationTaskMarkersSet[marker] = struct{}{}
	}

	err = enqueueAggregationTask(
		config.aggregationID,
		aggregationBatches,
		aggInterval,
		aggregationTaskMarkersSet,
		config.ownValidationBucket,
		config.aggregationTaskEnqueuer,
	)
	if err != nil {
		return err
	}

	// Ensure both task enqueuers have completed their asynchronous work before
	// allowing the process to exit
	config.intakeTaskEnqueuer.Stop()
	config.aggregationTaskEnqueuer.Stop()

	return nil
}

func enqueueAggregationTask(
	aggregationID string,
	readyBatches batchpath.List,
	aggregationWindow wftime.Interval,
	taskMarkers map[string]struct{},
	ownValidationBucket storage.Bucket,
	enqueuer task.Enqueuer,
) error {
	if len(readyBatches) == 0 {
		log.Info().Msg("no batches to aggregate")
		return nil
	}

	batches := []task.Batch{}

	batchCount := 0
	for _, batchPath := range readyBatches {
		batchCount++
		batches = append(batches, task.Batch{
			ID:   batchPath.ID,
			Time: wftime.Timestamp(batchPath.Time),
		})

		// All batches should have the same aggregation ID?
		if aggregationID != batchPath.AggregationID {
			return fmt.Errorf("found batch with aggregation ID %s, wanted %s", batchPath.AggregationID, aggregationID)
		}
	}

	aggregationTask := task.Aggregation{
		AggregationID:    aggregationID,
		AggregationStart: wftime.Timestamp(aggregationWindow.Begin),
		AggregationEnd:   wftime.Timestamp(aggregationWindow.End),
		Batches:          batches,
	}

	if _, ok := taskMarkers[aggregationTask.Marker()]; ok {
		log.Info().
			Str("aggregation ID", aggregationID).
			Msg("skipped aggregation task due to marker")
		aggregationsSkippedDueToMarker.Inc()
		return nil
	}

	log.Info().
		Str("aggregation ID", aggregationID).
		Str("aggregation window", aggregationWindow.String()).
		Int("batch count", batchCount).
		Msg("Scheduling aggregation task")

	enqueuer.Enqueue(aggregationTask, func(err error) {
		if err != nil {
			log.Err(err).
				Str("aggregation ID", aggregationID).
				Msg("failed to enqueue aggregation task")
			return
		}

		// Write a marker to cloud storage to ensure we don't schedule redundant
		// tasks
		if err := ownValidationBucket.WriteTaskMarker(aggregationTask.Marker()); err != nil {
			log.Err(err).
				Str("aggregation ID", aggregationID).
				Msg("failed to write aggregation task marker")
		}

		aggregationsStarted.Inc()
	})

	return nil
}

func enqueueIntakeTasks(
	readyBatches batchpath.List,
	taskMarkers map[string]struct{},
	ownValidationBucket storage.Bucket,
	enqueuer task.Enqueuer,
) error {
	skippedDueToMarker := 0
	scheduled := 0

	for _, batch := range readyBatches {
		intakeTask := task.IntakeBatch{
			AggregationID: batch.AggregationID,
			BatchID:       batch.ID,
			Date:          wftime.Timestamp(batch.Time),
		}

		if _, ok := taskMarkers[intakeTask.Marker()]; ok {
			skippedDueToMarker++
			intakesSkippedDueToMarker.Inc()
			continue
		}

		log.Info().
			Str("aggregation ID", batch.AggregationID).
			Str("batch", batch.String()).
			Msg("scheduling intake task for batch")

		scheduled++
		enqueuer.Enqueue(intakeTask, func(err error) {
			if err != nil {
				log.Err(err).
					Str("aggregation ID", batch.AggregationID).
					Msg("failed to enqueue intake task")
				return
			}
			// Write a marker to cloud storage to ensure we don't schedule
			// redundant tasks
			if err := ownValidationBucket.WriteTaskMarker(intakeTask.Marker()); err != nil {
				log.Err(err).
					Str("aggregation ID", batch.AggregationID).
					Msg("failed to write intake task marker")
				return
			}

			intakesStarted.Inc()
		})
	}

	log.Info().
		Int("skipped batches", skippedDueToMarker).
		Int("scheduled batches", scheduled).
		Msg("skipped and scheduled intake tasks")

	return nil
}
