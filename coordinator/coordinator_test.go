package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	help "github.com/featureform/helpers"
	"github.com/google/uuid"

	"github.com/featureform/metadata"
	"github.com/featureform/provider"
	pc "github.com/featureform/provider/provider_config"
	pt "github.com/featureform/provider/provider_type"
	"github.com/featureform/runner"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/joho/godotenv"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

func createSafeUUID() string {
	return strings.ReplaceAll(fmt.Sprintf("a%sa", uuid.New().String()), "-", "")
}

var testOfflineTableValues = [...]provider.ResourceRecord{
	provider.ResourceRecord{Entity: "a", Value: 1, TS: time.UnixMilli(0).UTC()},
	provider.ResourceRecord{Entity: "b", Value: 2, TS: time.UnixMilli(0).UTC()},
	provider.ResourceRecord{Entity: "c", Value: 3, TS: time.UnixMilli(0).UTC()},
	provider.ResourceRecord{Entity: "d", Value: 4, TS: time.UnixMilli(0).UTC()},
	provider.ResourceRecord{Entity: "e", Value: 5, TS: time.UnixMilli(0).UTC()},
}

var postgresConfig = pc.PostgresConfig{
	Host:     "localhost",
	Port:     "5432",
	Database: help.GetEnv("POSTGRES_DB", "postgres"),
	Username: help.GetEnv("POSTGRES_USER", "postgres"),
	Password: help.GetEnv("POSTGRES_PASSWORD", "password"),
	SSLMode:  "disable",
}

var redisPort = help.GetEnv("REDIS_INSECURE_PORT", "6379")
var redisHost = "localhost"

var etcdHost = "localhost"
var etcdPort = "2379"

type failingRunner struct {
	initialFails int
}

func (runner *failingRunner) Run() error {
	if runner.initialFails == 0 {
		return nil
	}
	runner.initialFails -= 1
	return fmt.Errorf("failure")
}

func TestRetryWithDelays(t *testing.T) {

	failsNever := failingRunner{0}
	failsOnce := failingRunner{1}
	alwaysFails := failingRunner{-1}

	if err := retryWithDelays("run runner", 5, time.Millisecond*1, failsNever.Run); err != nil {
		t.Fatalf("running retry with delays fails on never failing runner")
	}
	if err := retryWithDelays("run runner", 5, time.Millisecond*1, failsOnce.Run); err != nil {
		t.Fatalf("running retry with delays on 5 retries fails on once failing runner")
	}
	if err := retryWithDelays("run runner", 5, time.Millisecond*1, alwaysFails.Run); err == nil {
		t.Fatalf("running retry with doesn't fail on always failing runner")
	}
}

func startServ(t *testing.T) (*metadata.MetadataServer, string) {
	logger := zap.NewExample().Sugar()
	storageProvider := metadata.EtcdStorageProvider{
		metadata.EtcdConfig{
			Nodes: []metadata.EtcdNode{
				{etcdHost, etcdPort},
			},
		},
	}
	config := &metadata.Config{
		Logger:          logger,
		StorageProvider: storageProvider,
	}
	serv, err := metadata.NewMetadataServer(config)
	if err != nil {
		panic(err)
	}
	// listen on a random port
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	go func() {
		if err := serv.ServeOnListener(lis); err != nil {
			panic(err)
		}
	}()
	return serv, lis.Addr().String()
}

func createNewCoordinator(addr string) (*Coordinator, error) {
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		return nil, err
	}
	etcdConnect := fmt.Sprintf("%s:%s", etcdHost, etcdPort)
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdConnect}})
	if err != nil {
		return nil, err
	}
	memJobSpawner := MemoryJobSpawner{}
	return NewCoordinator(client, logger, cli, &memJobSpawner)
}

// may cause an error depending on kubernetes implementation
func TestKubernetesJobRunnerError(t *testing.T) {
	kubeJobSpawner := KubernetesJobSpawner{}
	if _, err := kubeJobSpawner.GetJobRunner("ghost_job", []byte{}, metadata.ResourceID{}); err == nil {
		t.Fatalf("did not trigger error getting nonexistent runner")
	}
}

func TestMemoryJobRunnerError(t *testing.T) {
	memJobSpawner := MemoryJobSpawner{}
	if _, err := memJobSpawner.GetJobRunner("ghost_job", []byte{}, metadata.ResourceID{}); err == nil {
		t.Fatalf("did not trigger error getting nonexistent runner")
	}
}

func TestRunSQLJobError(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	sourceGhostDependency := createSafeUUID()
	providerName := createSafeUUID()
	userName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: postgresConfig.Serialize(),
		},
		metadata.SourceDef{
			Name:        sourceGhostDependency,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.TransformationSource{
				TransformationType: metadata.SQLTransformationType{
					Query:   "{{ghost_source.}}",
					Sources: []metadata.NameVariant{{"ghost_source", ""}},
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create test metadata entries: %v", err)
	}
	transformSource, err := coord.Metadata.GetSourceVariant(context.Background(), metadata.NameVariant{sourceGhostDependency, ""})
	if err != nil {
		t.Fatalf("could not fetch created source variant: %v", err)
	}
	providerEntry, err := transformSource.FetchProvider(coord.Metadata, context.Background())
	if err != nil {
		t.Fatalf("could not fetch provider entry in metadata, provider entry not set: %v", err)
	}
	provider, err := provider.Get(pt.PostgresOffline, postgresConfig.Serialize())
	if err != nil {
		t.Fatalf("could not get provider: %v", err)
	}
	offlineProvider, err := provider.AsOfflineStore()
	if err != nil {
		t.Fatalf("could not get provider as offline store: %v", err)
	}
	sourceResourceID := metadata.ResourceID{sourceGhostDependency, "", metadata.SOURCE_VARIANT}
	if err := coord.runSQLTransformationJob(transformSource, sourceResourceID, offlineProvider, "", providerEntry); err == nil {
		t.Fatalf("did not catch error trying to run primary table job with no source table set")
	}
}

func TestFeatureMaterializeJobError(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	if err := coord.runFeatureMaterializeJob(metadata.ResourceID{"ghost_resource", "", metadata.FEATURE_VARIANT}, ""); err == nil {
		t.Fatalf("did not catch error when trying to materialize nonexistent feature")
	}
	liveAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	redisConfig := &pc.RedisConfig{
		Addr: liveAddr,
	}
	featureName := createSafeUUID()
	sourceName := createSafeUUID()
	originalTableName := createSafeUUID()
	if err := materializeFeatureWithProvider(coord.Metadata, postgresConfig.Serialize(), redisConfig.Serialized(), featureName, sourceName, originalTableName, ""); err != nil {
		t.Fatalf("could not create example feature, %v", err)
	}
	if err := coord.Metadata.SetStatus(context.Background(), metadata.ResourceID{featureName, "", metadata.FEATURE_VARIANT}, metadata.READY, ""); err != nil {
		t.Fatalf("could not set feature to ready")
	}
	if err := coord.runFeatureMaterializeJob(metadata.ResourceID{featureName, "", metadata.FEATURE_VARIANT}, ""); err == nil {
		t.Fatalf("did not catch error when trying to materialize feature already set to ready")
	}
	providerName := createSafeUUID()
	userName := createSafeUUID()
	sourceName = createSafeUUID()
	entityName := createSafeUUID()
	originalTableName = createSafeUUID()
	featureName = createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "INVALID_PROVIDER",
			Software:         "",
			Team:             "",
			SerializedConfig: []byte{},
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create metadata entries: %v", err)
	}
	if err := coord.Metadata.SetStatus(context.Background(), metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}, metadata.READY, ""); err != nil {
		t.Fatalf("could not set source variant to ready")
	}
	if err := coord.runFeatureMaterializeJob(metadata.ResourceID{featureName, "", metadata.FEATURE_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to run job with nonexistent provider")
	}
	providerName = createSafeUUID()
	userName = createSafeUUID()
	sourceName = createSafeUUID()
	entityName = createSafeUUID()
	originalTableName = createSafeUUID()
	featureName = createSafeUUID()
	defs = []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "REDIS_ONLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: redisConfig.Serialized(),
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create metadata entries: %v", err)
	}
	if err := coord.Metadata.SetStatus(context.Background(), metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}, metadata.READY, ""); err != nil {
		t.Fatalf("could not set source variant to ready")
	}
	if err := coord.runFeatureMaterializeJob(metadata.ResourceID{featureName, "", metadata.FEATURE_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to use online store as offline store")
	}
	providerName = createSafeUUID()
	offlineProviderName := createSafeUUID()
	userName = createSafeUUID()
	sourceName = createSafeUUID()
	entityName = createSafeUUID()
	originalTableName = createSafeUUID()
	featureName = createSafeUUID()
	defs = []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             offlineProviderName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: postgresConfig.Serialize(),
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "INVALID_PROVIDER",
			Software:         "",
			Team:             "",
			SerializedConfig: []byte{},
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    offlineProviderName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create metadata entries: %v", err)
	}
	if err := coord.Metadata.SetStatus(context.Background(), metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}, metadata.READY, ""); err != nil {
		t.Fatalf("could not set source variant to ready")
	}
	if err := coord.runFeatureMaterializeJob(metadata.ResourceID{featureName, "", metadata.FEATURE_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to get invalid feature provider")
	}
}

func TestTrainingSetJobError(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	if err := coord.runTrainingSetJob(metadata.ResourceID{"ghost_training_set", "", metadata.TRAINING_SET_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to run job for nonexistent training set")
	}
	providerName := createSafeUUID()
	userName := createSafeUUID()
	sourceName := createSafeUUID()
	entityName := createSafeUUID()
	labelName := createSafeUUID()
	originalTableName := createSafeUUID()
	featureName := createSafeUUID()
	tsName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "INVALID_PROVIDER",
			Software:         "",
			Team:             "",
			SerializedConfig: []byte{},
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.LabelDef{
			Name:        labelName,
			Variant:     "",
			Description: "",
			Type:        string(provider.Int),
			Source:      metadata.NameVariant{sourceName, ""},
			Entity:      entityName,
			Owner:       userName,
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: "",
		},
		metadata.TrainingSetDef{
			Name:        tsName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Label:       metadata.NameVariant{labelName, ""},
			Features:    []metadata.NameVariant{{featureName, ""}},
			Schedule:    "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create metadata entries: %v", err)
	}
	if err := coord.runTrainingSetJob(metadata.ResourceID{tsName, "", metadata.TRAINING_SET_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to run job with nonexistent provider")
	}
	providerName = createSafeUUID()
	userName = createSafeUUID()
	sourceName = createSafeUUID()
	entityName = createSafeUUID()
	labelName = createSafeUUID()
	originalTableName = createSafeUUID()
	featureName = createSafeUUID()
	tsName = createSafeUUID()
	liveAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	redisConfig := &pc.RedisConfig{
		Addr: liveAddr,
	}
	defs = []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "REDIS_ONLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: redisConfig.Serialized(),
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.LabelDef{
			Name:        labelName,
			Variant:     "",
			Description: "",
			Type:        string(provider.Int),
			Source:      metadata.NameVariant{sourceName, ""},
			Entity:      entityName,
			Owner:       userName,
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: "",
		},
		metadata.TrainingSetDef{
			Name:        tsName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Label:       metadata.NameVariant{labelName, ""},
			Features:    []metadata.NameVariant{{featureName, ""}},
			Schedule:    "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create metadata entries: %v", err)
	}
	if err := coord.runTrainingSetJob(metadata.ResourceID{tsName, "", metadata.TRAINING_SET_VARIANT}, ""); err == nil {
		t.Fatalf("did not trigger error trying to convert online provider to offline")
	}
}

func TestRunPrimaryTableJobError(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	sourceNoPrimaryNameSet := createSafeUUID()
	providerName := createSafeUUID()
	userName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: postgresConfig.Serialize(),
		},
		metadata.SourceDef{
			Name:        sourceNoPrimaryNameSet,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: "",
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create test metadata entries")
	}
	transformSource, err := coord.Metadata.GetSourceVariant(context.Background(), metadata.NameVariant{sourceNoPrimaryNameSet, ""})
	if err != nil {
		t.Fatalf("could not fetch created source variant: %v", err)
	}
	provider, err := provider.Get(pt.PostgresOffline, postgresConfig.Serialize())
	if err != nil {
		t.Fatalf("could not get provider: %v", err)
	}
	offlineProvider, err := provider.AsOfflineStore()
	if err != nil {
		t.Fatalf("could not get provider as offline store: %v", err)
	}
	sourceResourceID := metadata.ResourceID{sourceNoPrimaryNameSet, "", metadata.SOURCE_VARIANT}
	if err := coord.runPrimaryTableJob(transformSource, sourceResourceID, offlineProvider, ""); err == nil {
		t.Fatalf("did not catch error trying to run primary table job with no source table set")
	}
	sourceNoActualPrimaryTable := createSafeUUID()
	newProviderName := createSafeUUID()
	newUserName := createSafeUUID()
	newDefs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: newUserName,
		},
		metadata.ProviderDef{
			Name:             newProviderName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: postgresConfig.Serialize(),
		},
		metadata.SourceDef{
			Name:        sourceNoActualPrimaryTable,
			Variant:     "",
			Description: "",
			Owner:       newUserName,
			Provider:    newProviderName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: "ghost_primary_table",
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), newDefs); err != nil {
		t.Fatalf("could not create test metadata entries: %v", err)
	}
	newTransformSource, err := coord.Metadata.GetSourceVariant(context.Background(), metadata.NameVariant{sourceNoActualPrimaryTable, ""})
	if err != nil {
		t.Fatalf("could not fetch created source variant: %v", err)
	}
	newSourceResourceID := metadata.ResourceID{sourceNoActualPrimaryTable, "", metadata.SOURCE_VARIANT}
	if err := coord.runPrimaryTableJob(newTransformSource, newSourceResourceID, offlineProvider, ""); err == nil {
		t.Fatalf("did not catch error trying to create primary table when no source table exists in database")
	}
}

func TestMapNameVariantsToTablesError(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	ghostResourceName := createSafeUUID()
	ghostNameVariants := []metadata.NameVariant{{ghostResourceName, ""}}
	if _, err := coord.mapNameVariantsToTables(ghostNameVariants); err == nil {
		t.Fatalf("did not catch error creating map from nonexistent resource")
	}
	sourceNotReady := createSafeUUID()
	providerName := createSafeUUID()
	tableName := createSafeUUID()
	userName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: []byte{},
		},
		metadata.SourceDef{
			Name:        sourceNotReady,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: tableName,
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), defs); err != nil {
		t.Fatalf("could not create test metadata entries")
	}
	notReadyNameVariants := []metadata.NameVariant{{sourceNotReady, ""}}
	if _, err := coord.mapNameVariantsToTables(notReadyNameVariants); err == nil {
		t.Fatalf("did not catch error creating map from not ready resource")
	}
}

func TestRegisterSourceJobErrors(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	coord, err := createNewCoordinator(addr)
	if err != nil {
		t.Fatalf("could not create new basic coordinator")
	}
	defer coord.Metadata.Close()
	ghostResourceName := createSafeUUID()
	ghostResourceID := metadata.ResourceID{ghostResourceName, "", metadata.SOURCE_VARIANT}
	if err := coord.runRegisterSourceJob(ghostResourceID, ""); err == nil {
		t.Fatalf("did not catch error registering nonexistent resource")
	}
	sourceWithoutProvider := createSafeUUID()
	ghostProviderName := createSafeUUID()
	ghostTableName := createSafeUUID()
	userName := createSafeUUID()
	providerErrorDefs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             ghostProviderName,
			Description:      "",
			Type:             "GHOST_PROVIDER",
			Software:         "",
			Team:             "",
			SerializedConfig: []byte{},
		},
		metadata.SourceDef{
			Name:        sourceWithoutProvider,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    ghostProviderName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: ghostTableName,
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), providerErrorDefs); err != nil {
		t.Fatalf("could not create test metadata entries")
	}
	sourceWithoutProviderResourceID := metadata.ResourceID{sourceWithoutProvider, "", metadata.SOURCE_VARIANT}
	if err := coord.runRegisterSourceJob(sourceWithoutProviderResourceID, ""); err == nil {
		t.Fatalf("did not catch error registering registering resource without provider in offline store")
	}
	sourceWithoutOfflineProvider := createSafeUUID()
	onlineProviderName := createSafeUUID()
	newTableName := createSafeUUID()
	newUserName := createSafeUUID()
	liveAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	redisConfig := &pc.RedisConfig{
		Addr: liveAddr,
	}
	serialRedisConfig := redisConfig.Serialized()
	onlineErrorDefs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: newUserName,
		},
		metadata.ProviderDef{
			Name:             onlineProviderName,
			Description:      "",
			Type:             "REDIS_ONLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: serialRedisConfig,
		},
		metadata.SourceDef{
			Name:        sourceWithoutOfflineProvider,
			Variant:     "",
			Description: "",
			Owner:       newUserName,
			Provider:    onlineProviderName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: newTableName,
				},
			},
			Schedule: "",
		},
	}
	if err := coord.Metadata.CreateAll(context.Background(), onlineErrorDefs); err != nil {
		t.Fatalf("could not create test metadata entries")
	}
	sourceWithOnlineProvider := metadata.ResourceID{sourceWithoutOfflineProvider, "", metadata.SOURCE_VARIANT}
	if err := coord.runRegisterSourceJob(sourceWithOnlineProvider, ""); err == nil {
		t.Fatalf("did not catch error registering registering resource with online provider")
	}
}

func TestTemplateReplace(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	err := godotenv.Load(".env")
	if err != nil {
		fmt.Println(err)
	}

	bigQueryConfig := getBigQueryConfig(t)
	bqPrefix := fmt.Sprintf("%s.%s", bigQueryConfig.ProjectId, bigQueryConfig.DatasetId)

	cases := []struct {
		name            string
		provider        pt.Type
		config          pc.SerializedConfig
		templateString  string
		replacements    map[string]string
		expectedResults string
		expectedFailure bool
	}{
		{
			"PostgresSuccess",
			pt.PostgresOffline,
			postgresConfig.Serialize(),
			"Some example text {{name1.variant1}} and more {{name2.variant2}}",
			map[string]string{"name1.variant1": "replacement1", "name2.variant2": "replacement2"},
			"Some example text \"replacement1\" and more \"replacement2\"",
			false,
		},
		{
			"PostgresFailure",
			pt.PostgresOffline,
			postgresConfig.Serialize(),
			"Some example text {{name1.variant1}} and more {{name2.variant2}}",
			map[string]string{"name1.variant1": "replacement1", "name3.variant3": "replacement2"},
			"",
			true,
		},
		{
			"BigQuerySuccess",
			pt.BigQueryOffline,
			bigQueryConfig.Serialize(),
			"Some example text {{name1.variant1}} and more {{name2.variant2}}",
			map[string]string{"name1.variant1": "replacement1", "name2.variant2": "replacement2"},
			fmt.Sprintf("Some example text `%s.replacement1` and more `%s.replacement2`", bqPrefix, bqPrefix),
			false,
		},
		{
			"BigQueryFailure",
			pt.BigQueryOffline,
			bigQueryConfig.Serialize(),
			"Some example text {{name1.variant1}} and more {{name2.variant2}}",
			map[string]string{"name1.variant1": "replacement1", "name3.variant3": "replacement2"},
			"",
			true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			offlineProvider := getOfflineStore(t, tt.provider, tt.config)
			result, err := templateReplace(tt.templateString, tt.replacements, offlineProvider)
			if !tt.expectedFailure && err != nil {
				t.Fatalf("template replace did not run correctly: %v", err)
			}
			if !tt.expectedFailure && result != tt.expectedResults {
				t.Fatalf("template replace did not replace values correctly. Expected %s, got %s", tt.expectedResults, result)
			}
		})
	}
}

func getOfflineStore(t *testing.T, providerName pt.Type, config pc.SerializedConfig) provider.OfflineStore {
	provider, err := provider.Get(providerName, config)
	if err != nil {
		t.Fatalf("could not get provider: %v", err)
	}
	offlineProvider, err := provider.AsOfflineStore()
	if err != nil {
		t.Fatalf("could not get provider as offline store: %v", err)
	}
	return offlineProvider
}

func getBigQueryConfig(t *testing.T) pc.BigQueryConfig {
	bigqueryCredentials := os.Getenv("BIGQUERY_CREDENTIALS")
	JSONCredentials, err := ioutil.ReadFile(bigqueryCredentials)
	if err != nil {
		panic(fmt.Errorf("cannot find big query credentials: %v", err))
	}

	bigQueryDatasetId := strings.Replace(strings.ToUpper(uuid.NewString()), "-", "_", -1)
	os.Setenv("BIGQUERY_DATASET_ID", bigQueryDatasetId)
	t.Log("BigQuery Dataset: ", bigQueryDatasetId)

	var serializedCreds map[string]interface{}
	err = json.Unmarshal(JSONCredentials, &serializedCreds)
	if err != nil {
		panic(fmt.Errorf("cannot deserialize big query credentials: %v", err))
	}

	var bigQueryConfig = pc.BigQueryConfig{
		ProjectId:   os.Getenv("BIGQUERY_PROJECT_ID"),
		DatasetId:   os.Getenv("BIGQUERY_DATASET_ID"),
		Credentials: serializedCreds,
	}

	return bigQueryConfig
}

func TestCoordinatorCalls(t *testing.T) {
	if testing.Short() {
		return
	}
	serv, addr := startServ(t)
	defer serv.Stop()
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		t.Fatalf("could not set up metadata client: %v", err)
	}
	defer client.Close()
	if err := testCoordinatorMaterializeFeature(addr); err != nil {
		t.Fatalf("coordinator could not materialize feature: %v", err)
	}
	if err := testCoordinatorTrainingSet(addr); err != nil {
		t.Fatalf("coordinator could not create training set: %v", err)
	}
	if err := testRegisterPrimaryTableFromSource(addr); err != nil {
		t.Fatalf("coordinator could not register primary table from source: %v", err)
	}
	if err := testRegisterTransformationFromSource(addr); err != nil {
		t.Fatalf("coordinator could not register transformation from source and transformation: %v", err)
	}
	// if err := testScheduleTrainingSet(addr); err != nil {
	// 	t.Fatalf("coordinator could not schedule training set to be updated: %v", err)
	// }
	// if err := testScheduleTransformation(addr); err != nil {
	// 	t.Fatalf("coordinator could not schedule transformation to be updated: %v", err)
	// }
	// if err := testScheduleFeatureMaterialization(addr); err != nil {
	// 	t.Fatalf("coordinator could not schedule materialization to be updated: %v", err)
	// }
}

func materializeFeatureWithProvider(client *metadata.Client, offlineConfig pc.SerializedConfig, onlineConfig pc.SerializedConfig, featureName string, sourceName string, originalTableName string, schedule string) error {
	offlineProviderName := createSafeUUID()
	onlineProviderName := createSafeUUID()
	userName := createSafeUUID()
	entityName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             offlineProviderName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: offlineConfig,
		},
		metadata.ProviderDef{
			Name:             onlineProviderName,
			Description:      "",
			Type:             "REDIS_ONLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: onlineConfig,
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    offlineProviderName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
			Schedule: "",
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    onlineProviderName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
			Schedule: schedule,
		},
	}
	if err := client.CreateAll(context.Background(), defs); err != nil {
		return err
	}
	return nil
}

func createSourceWithProvider(client *metadata.Client, config pc.SerializedConfig, sourceName string, tableName string) error {
	userName := createSafeUUID()
	providerName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: config,
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: tableName,
				},
			},
		},
	}
	if err := client.CreateAll(context.Background(), defs); err != nil {
		return err
	}
	return nil
}

func createTransformationWithProvider(client *metadata.Client, config pc.SerializedConfig, sourceName string, transformationQuery string, sources []metadata.NameVariant, schedule string) error {
	userName := createSafeUUID()
	providerName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: config,
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.TransformationSource{
				TransformationType: metadata.SQLTransformationType{
					Query:   transformationQuery,
					Sources: sources,
				},
			},
			Schedule: schedule,
		},
	}
	if err := client.CreateAll(context.Background(), defs); err != nil {
		return err
	}
	return nil
}

func createTrainingSetWithProvider(client *metadata.Client, config pc.SerializedConfig, sourceName string, featureName string, labelName string, tsName string, originalTableName string, schedule string) error {
	providerName := createSafeUUID()
	userName := createSafeUUID()
	entityName := createSafeUUID()
	defs := []metadata.ResourceDef{
		metadata.UserDef{
			Name: userName,
		},
		metadata.ProviderDef{
			Name:             providerName,
			Description:      "",
			Type:             "POSTGRES_OFFLINE",
			Software:         "",
			Team:             "",
			SerializedConfig: config,
		},
		metadata.EntityDef{
			Name:        entityName,
			Description: "",
		},
		metadata.SourceDef{
			Name:        sourceName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Definition: metadata.PrimaryDataSource{
				Location: metadata.SQLTable{
					Name: originalTableName,
				},
			},
		},
		metadata.LabelDef{
			Name:        labelName,
			Variant:     "",
			Description: "",
			Type:        string(provider.Int),
			Source:      metadata.NameVariant{sourceName, ""},
			Entity:      entityName,
			Owner:       userName,
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
		},
		metadata.FeatureDef{
			Name:        featureName,
			Variant:     "",
			Source:      metadata.NameVariant{sourceName, ""},
			Type:        string(provider.Int),
			Entity:      entityName,
			Owner:       userName,
			Description: "",
			Provider:    providerName,
			Location: metadata.ResourceVariantColumns{
				Entity: "entity",
				Value:  "value",
				TS:     "ts",
			},
		},
		metadata.TrainingSetDef{
			Name:        tsName,
			Variant:     "",
			Description: "",
			Owner:       userName,
			Provider:    providerName,
			Label:       metadata.NameVariant{labelName, ""},
			Features:    []metadata.NameVariant{{featureName, ""}},
			Schedule:    schedule,
		},
	}
	if err := client.CreateAll(context.Background(), defs); err != nil {
		return err
	}
	return nil
}

func testCoordinatorTrainingSet(addr string) error {
	if err := runner.RegisterFactory(string(runner.CREATE_TRAINING_SET), runner.TrainingSetRunnerFactory); err != nil {
		return fmt.Errorf("Failed to register training set runner factory: %v", err)
	}
	defer runner.UnregisterFactory(string(runner.CREATE_TRAINING_SET))
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		return fmt.Errorf("Failed to connect: %v", err)
	}
	defer client.Close()
	etcdConnect := fmt.Sprintf("%s:%s", etcdHost, etcdPort)
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdConnect}})
	if err != nil {
		return err
	}
	defer cli.Close()
	featureName := createSafeUUID()
	labelName := createSafeUUID()
	tsName := createSafeUUID()
	serialPGConfig := postgresConfig.Serialize()
	my_provider, err := provider.Get(pt.PostgresOffline, serialPGConfig)
	if err != nil {
		return fmt.Errorf("could not get provider: %v", err)
	}
	my_offline, err := my_provider.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("could not get provider as offline store: %v", err)
	}
	offline_feature := provider.ResourceID{Name: featureName, Variant: "", Type: provider.Feature}
	originalTableName := createSafeUUID()
	if err := CreateOriginalPostgresTable(originalTableName); err != nil {
		return err
	}
	sourceName := createSafeUUID()
	if err := createTrainingSetWithProvider(client, serialPGConfig, sourceName, featureName, labelName, tsName, originalTableName, ""); err != nil {
		return fmt.Errorf("could not create training set %v", err)
	}
	ctx := context.Background()
	tsID := metadata.ResourceID{Name: tsName, Variant: "", Type: metadata.TRAINING_SET_VARIANT}
	tsCreated, err := client.GetTrainingSetVariant(ctx, metadata.NameVariant{Name: tsName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get training set")
	}
	if tsCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Training set not set to created with no coordinator running")
	}
	memJobSpawner := MemoryJobSpawner{}
	coord, err := NewCoordinator(client, logger, cli, &memJobSpawner)
	if err != nil {
		return fmt.Errorf("Failed to set up coordinator")
	}
	sourceID := metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}
	if err := coord.ExecuteJob(metadata.GetJobKey(sourceID)); err != nil {
		return err
	}
	featureID := metadata.ResourceID{Name: featureName, Variant: "", Type: metadata.FEATURE_VARIANT}
	if err := coord.ExecuteJob(metadata.GetJobKey(featureID)); err != nil {
		return err
	}
	labelID := metadata.ResourceID{Name: labelName, Variant: "", Type: metadata.LABEL_VARIANT}
	if err := coord.ExecuteJob(metadata.GetJobKey(labelID)); err != nil {
		return err
	}
	featureTable, err := my_offline.GetResourceTable(offline_feature)
	if err != nil {
		return fmt.Errorf("could not create feature table: %v", err)
	}
	for _, value := range testOfflineTableValues {
		if err := featureTable.Write(value); err != nil {
			return fmt.Errorf("could not write to offline feature table")
		}
	}
	offline_label := provider.ResourceID{Name: labelName, Variant: "", Type: provider.Label}
	labelTable, err := my_offline.GetResourceTable(offline_label)
	if err != nil {
		return fmt.Errorf("could not create label table: %v", err)
	}
	for _, value := range testOfflineTableValues {
		if err := labelTable.Write(value); err != nil {
			return fmt.Errorf("could not write to offline label table")
		}
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(tsID)); err != nil {
		return err
	}
	startWaitDelete := time.Now()
	elapsed := time.Since(startWaitDelete)
	for has, _ := coord.hasJob(tsID); has && elapsed < time.Duration(10)*time.Second; has, _ = coord.hasJob(tsID) {
		time.Sleep(1 * time.Second)
		elapsed = time.Since(startWaitDelete)
		fmt.Printf("waiting for job %v to be deleted\n", tsID)
	}
	if elapsed >= time.Duration(10)*time.Second {
		return fmt.Errorf("timed out waiting for job to delete")
	}
	ts_complete, err := client.GetTrainingSetVariant(ctx, metadata.NameVariant{Name: tsName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get training set variant")
	}
	if metadata.READY != ts_complete.Status() {
		return fmt.Errorf("Training set not set to ready once job completes")
	}
	if err := coord.runTrainingSetJob(tsID, ""); err == nil {
		return fmt.Errorf("run training set job did not trigger error when tried to create training set that already exists")
	}
	providerTsID := provider.ResourceID{Name: tsID.Name, Variant: tsID.Variant, Type: provider.TrainingSet}
	tsIterator, err := my_offline.GetTrainingSet(providerTsID)
	if err != nil {
		return fmt.Errorf("Coordinator did not create training set")
	}

	for i := 0; tsIterator.Next(); i++ {
		retrievedFeatures := tsIterator.Features()
		retrievedLabel := tsIterator.Label()
		if !reflect.DeepEqual(retrievedFeatures[0], testOfflineTableValues[i].Value) {
			return fmt.Errorf("Features not copied into training set")
		}
		if !reflect.DeepEqual(retrievedLabel, testOfflineTableValues[i].Value) {
			return fmt.Errorf("Label not copied into training set")
		}

	}
	return nil
}

func testCoordinatorMaterializeFeature(addr string) error {
	if err := runner.RegisterFactory(string(runner.COPY_TO_ONLINE), runner.MaterializedChunkRunnerFactory); err != nil {
		return fmt.Errorf("Failed to register training set runner factory: %v", err)
	}
	defer runner.UnregisterFactory(string(runner.COPY_TO_ONLINE))
	if err := runner.RegisterFactory(string(runner.MATERIALIZE), runner.MaterializeRunnerFactory); err != nil {
		return fmt.Errorf("Failed to register training set runner factory: %v", err)
	}
	defer runner.UnregisterFactory(string(runner.MATERIALIZE))
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		return fmt.Errorf("Failed to connect: %v", err)
	}
	defer client.Close()
	etcdConnect := fmt.Sprintf("%s:%s", etcdHost, etcdPort)
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdConnect}})
	if err != nil {
		return err
	}
	defer cli.Close()
	serialPGConfig := postgresConfig.Serialize()
	liveAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	redisConfig := &pc.RedisConfig{
		Addr: liveAddr,
	}
	serialRedisConfig := redisConfig.Serialized()
	p, err := provider.Get(pt.RedisOnline, serialRedisConfig)
	if err != nil {
		return fmt.Errorf("could not get online provider: %v", err)
	}
	onlineStore, err := p.AsOnlineStore()
	if err != nil {
		return fmt.Errorf("could not get provider as online store")
	}
	featureName := createSafeUUID()
	sourceName := createSafeUUID()
	originalTableName := createSafeUUID()
	if err := CreateOriginalPostgresTable(originalTableName); err != nil {
		return err
	}
	if err := materializeFeatureWithProvider(client, serialPGConfig, serialRedisConfig, featureName, sourceName, originalTableName, ""); err != nil {
		return fmt.Errorf("could not create online feature in metadata: %v", err)
	}
	if err := client.SetStatus(context.Background(), metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}, metadata.READY, ""); err != nil {
		return err
	}
	featureID := metadata.ResourceID{Name: featureName, Variant: "", Type: metadata.FEATURE_VARIANT}
	sourceID := metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}
	featureCreated, err := client.GetFeatureVariant(context.Background(), metadata.NameVariant{Name: featureName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get feature: %v", err)
	}
	if featureCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Feature not set to created with no coordinator running")
	}
	memJobSpawner := MemoryJobSpawner{}
	coord, err := NewCoordinator(client, logger, cli, &memJobSpawner)
	if err != nil {
		return fmt.Errorf("Failed to set up coordinator")
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(sourceID)); err != nil {
		return err
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(featureID)); err != nil {
		return err
	}
	startWaitDelete := time.Now()
	elapsed := time.Since(startWaitDelete)
	for has, _ := coord.hasJob(featureID); has && elapsed < time.Duration(10)*time.Second; has, _ = coord.hasJob(featureID) {
		time.Sleep(1 * time.Second)
		elapsed = time.Since(startWaitDelete)
		fmt.Printf("waiting for job %v to be deleted\n", featureID)
	}
	if elapsed >= time.Duration(10)*time.Second {
		return fmt.Errorf("timed out waiting for job to delete")
	}
	featureComplete, err := client.GetFeatureVariant(context.Background(), metadata.NameVariant{Name: featureName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get feature variant")
	}
	if metadata.READY != featureComplete.Status() {
		return fmt.Errorf("Feature not set to ready once job completes")
	}
	resourceTable, err := onlineStore.GetTable(featureName, "")
	if err != nil {
		return err
	}
	for _, record := range testOfflineTableValues {
		value, err := resourceTable.Get(record.Entity)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(value, record.Value) {
			return fmt.Errorf("Feature value did not materialize")
		}
	}
	return nil
}

func CreateOriginalPostgresTable(tableName string) error {
	url := fmt.Sprintf("postgres://%s:%s@%s:%s/%s", postgresConfig.Username, postgresConfig.Password, postgresConfig.Host, postgresConfig.Port, postgresConfig.Database)
	ctx := context.Background()
	conn, err := pgxpool.Connect(ctx, url)
	if err != nil {
		return err
	}
	createTableQuery := fmt.Sprintf("CREATE TABLE %s (entity VARCHAR, value INT, ts TIMESTAMPTZ)", sanitize(tableName))
	if _, err := conn.Exec(context.Background(), createTableQuery); err != nil {
		return err
	}
	for _, record := range testOfflineTableValues {
		upsertQuery := fmt.Sprintf("INSERT INTO %s (entity, value, ts) VALUES ($1, $2, $3)", sanitize(tableName))
		if _, err := conn.Exec(context.Background(), upsertQuery, record.Entity, record.Value, record.TS); err != nil {
			return err
		}
	}
	return nil
}

func testRegisterPrimaryTableFromSource(addr string) error {
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		return fmt.Errorf("Failed to connect: %v", err)
	}
	defer client.Close()
	etcdConnect := fmt.Sprintf("%s:%s", etcdHost, etcdPort)
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdConnect}})
	if err != nil {
		return err
	}
	defer cli.Close()
	tableName := createSafeUUID()
	serialPGConfig := postgresConfig.Serialize()
	myProvider, err := provider.Get(pt.PostgresOffline, serialPGConfig)
	if err != nil {
		return fmt.Errorf("could not get provider: %v", err)
	}
	myOffline, err := myProvider.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("could not get provider as offline store: %v", err)
	}
	if err := CreateOriginalPostgresTable(tableName); err != nil {
		return fmt.Errorf("Could not create non-featureform source table: %v", err)
	}
	sourceName := createSafeUUID()
	if err := createSourceWithProvider(client, pc.SerializedConfig(serialPGConfig), sourceName, tableName); err != nil {
		return fmt.Errorf("could not register source in metadata: %v", err)
	}
	sourceCreated, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: sourceName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source: %v", err)
	}
	if sourceCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Source not set to created with no coordinator running")
	}
	sourceID := metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}
	memJobSpawner := MemoryJobSpawner{}
	coord, err := NewCoordinator(client, logger, cli, &memJobSpawner)
	if err != nil {
		return fmt.Errorf("Failed to set up coordinator")
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(sourceID)); err != nil {
		return err
	}
	startWaitDelete := time.Now()
	elapsed := time.Since(startWaitDelete)
	for has, _ := coord.hasJob(sourceID); has && elapsed < time.Duration(10)*time.Second; has, _ = coord.hasJob(sourceID) {
		time.Sleep(1 * time.Second)
		elapsed = time.Since(startWaitDelete)
		fmt.Printf("waiting for job %v to be deleted\n", sourceID)
	}
	if elapsed >= time.Duration(10)*time.Second {
		return fmt.Errorf("timed out waiting for job to delete")
	}
	sourceComplete, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: sourceName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source variant")
	}
	if metadata.READY != sourceComplete.Status() {
		return fmt.Errorf("source variant not set to ready once job completes")
	}
	providerSourceID := provider.ResourceID{Name: sourceName, Variant: "", Type: provider.Primary}
	primaryTable, err := myOffline.GetPrimaryTable(providerSourceID)
	if err != nil {
		return fmt.Errorf("Coordinator did not create primary table")
	}
	primaryTableName, err := provider.GetPrimaryTableName(providerSourceID)
	if err != nil {
		return fmt.Errorf("invalid table name: %v", err)
	}
	if primaryTable.GetName() != primaryTableName {
		return fmt.Errorf("Primary table did not copy name")
	}
	numRows, err := primaryTable.NumRows()
	if err != nil {
		return fmt.Errorf("Could not get num rows from primary table")
	}
	if int(numRows) != len(testOfflineTableValues) {
		return fmt.Errorf("primary table did not copy correct number of rows")
	}
	primaryTableIterator, err := primaryTable.IterateSegment(int64(len(testOfflineTableValues)))
	if err != nil {
		return err
	}
	i := 0
	for ; primaryTableIterator.Next(); i++ {
		if primaryTableIterator.Err() != nil {
			return err
		}
		primaryTableRow := primaryTableIterator.Values()
		values := reflect.ValueOf(testOfflineTableValues[i])
		for j := 0; j < values.NumField(); j++ {
			if primaryTableRow[j] != values.Field(j).Interface() {
				return fmt.Errorf("Primary table value does not match original value")
			}
		}
	}
	if i != len(testOfflineTableValues) {
		return fmt.Errorf("primary table did not copy all rows")
	}
	return nil
}

func testRegisterTransformationFromSource(addr string) error {
	if err := runner.RegisterFactory(string(runner.CREATE_TRANSFORMATION), runner.CreateTransformationRunnerFactory); err != nil {
		return fmt.Errorf("Failed to register training set runner factory: %v", err)
	}
	defer runner.UnregisterFactory(string(runner.CREATE_TRANSFORMATION))
	logger := zap.NewExample().Sugar()
	client, err := metadata.NewClient(addr, logger)
	if err != nil {
		return fmt.Errorf("Failed to connect: %v", err)
	}
	defer client.Close()
	etcdConnect := fmt.Sprintf("%s:%s", etcdHost, etcdPort)
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdConnect}})
	if err != nil {
		return err
	}
	defer cli.Close()
	tableName := createSafeUUID()
	serialPGConfig := postgresConfig.Serialize()
	myProvider, err := provider.Get(pt.PostgresOffline, serialPGConfig)
	if err != nil {
		return fmt.Errorf("could not get provider: %v", err)
	}
	myOffline, err := myProvider.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("could not get provider as offline store: %v", err)
	}
	if err := CreateOriginalPostgresTable(tableName); err != nil {
		return fmt.Errorf("Could not create non-featureform source table: %v", err)
	}
	sourceName := strings.Replace(createSafeUUID(), "-", "", -1)
	if err := createSourceWithProvider(client, pc.SerializedConfig(serialPGConfig), sourceName, tableName); err != nil {
		return fmt.Errorf("could not register source in metadata: %v", err)
	}
	sourceCreated, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: sourceName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source: %v", err)
	}
	if sourceCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Source not set to created with no coordinator running")
	}
	sourceID := metadata.ResourceID{Name: sourceName, Variant: "", Type: metadata.SOURCE_VARIANT}
	memJobSpawner := MemoryJobSpawner{}
	coord, err := NewCoordinator(client, logger, cli, &memJobSpawner)
	if err != nil {
		return fmt.Errorf("Failed to set up coordinator")
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(sourceID)); err != nil {
		return err
	}
	sourceComplete, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: sourceName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source variant")
	}
	if metadata.READY != sourceComplete.Status() {
		return fmt.Errorf("source variant not set to ready once job completes")
	}
	transformationQuery := fmt.Sprintf("SELECT * FROM {{%s.}}", sourceName)
	transformationName := strings.Replace(createSafeUUID(), "-", "", -1)
	transformationID := metadata.ResourceID{Name: transformationName, Variant: "", Type: metadata.SOURCE_VARIANT}
	sourceNameVariants := []metadata.NameVariant{{Name: sourceName, Variant: ""}}
	if err := createTransformationWithProvider(client, serialPGConfig, transformationName, transformationQuery, sourceNameVariants, ""); err != nil {
		return err
	}
	transformationCreated, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: transformationName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get transformation: %v", err)
	}
	if transformationCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Transformation not set to created with no coordinator running")
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(transformationID)); err != nil {
		return err
	}
	transformationComplete, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: transformationName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source variant")
	}
	if metadata.READY != transformationComplete.Status() {
		return fmt.Errorf("transformation variant not set to ready once job completes")
	}
	providerTransformationID := provider.ResourceID{Name: transformationName, Variant: "", Type: provider.Transformation}
	transformationTable, err := myOffline.GetTransformationTable(providerTransformationID)
	if err != nil {
		return err
	}
	transformationTableName, err := provider.GetPrimaryTableName(providerTransformationID)
	if err != nil {
		return fmt.Errorf("invalid transformation table name: %v", err)
	}
	if transformationTable.GetName() != transformationTableName {
		return fmt.Errorf("Transformation table did not copy name")
	}
	numRows, err := transformationTable.NumRows()
	if err != nil {
		return fmt.Errorf("Could not get num rows from transformation table")
	}
	if int(numRows) != len(testOfflineTableValues) {
		return fmt.Errorf("transformation table did not copy correct number of rows")
	}
	transformationIterator, err := transformationTable.IterateSegment(int64(len(testOfflineTableValues)))
	if err != nil {
		return err
	}
	i := 0
	for ; transformationIterator.Next(); i++ {
		if transformationIterator.Err() != nil {
			return err
		}
		transformationTableRow := transformationIterator.Values()
		values := reflect.ValueOf(testOfflineTableValues[i])
		for j := 0; j < values.NumField(); j++ {
			if transformationTableRow[j] != values.Field(j).Interface() {
				return fmt.Errorf("Transformation table value does not match original value")
			}
		}
	}
	if i != len(testOfflineTableValues) {
		return fmt.Errorf("transformation table did not copy all rows")
	}

	joinTransformationQuery := fmt.Sprintf("SELECT {{%s.}}.entity, {{%s.}}.value, {{%s.}}.ts FROM {{%s.}} INNER JOIN {{%s.}} ON {{%s.}}.entity = {{%s.}}.entity", sourceName, sourceName, sourceName, sourceName, transformationName, sourceName, transformationName)
	joinTransformationName := strings.Replace(createSafeUUID(), "-", "", -1)
	joinTransformationID := metadata.ResourceID{Name: joinTransformationName, Variant: "", Type: metadata.SOURCE_VARIANT}
	joinSourceNameVariants := []metadata.NameVariant{{Name: sourceName, Variant: ""}, {Name: transformationName, Variant: ""}}
	if err := createTransformationWithProvider(client, serialPGConfig, joinTransformationName, joinTransformationQuery, joinSourceNameVariants, ""); err != nil {
		return err
	}
	joinTransformationCreated, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: joinTransformationName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get transformation: %v", err)
	}
	if joinTransformationCreated.Status() != metadata.CREATED {
		return fmt.Errorf("Transformation not set to created with no coordinator running")
	}
	if err := coord.ExecuteJob(metadata.GetJobKey(joinTransformationID)); err != nil {
		return err
	}
	joinTransformationComplete, err := client.GetSourceVariant(context.Background(), metadata.NameVariant{Name: joinTransformationName, Variant: ""})
	if err != nil {
		return fmt.Errorf("could not get source variant")
	}
	if metadata.READY != joinTransformationComplete.Status() {
		return fmt.Errorf("transformation variant not set to ready once job completes")
	}
	providerJoinTransformationID := provider.ResourceID{Name: transformationName, Variant: "", Type: provider.Transformation}
	joinTransformationTable, err := myOffline.GetTransformationTable(providerJoinTransformationID)
	if err != nil {
		return err
	}
	transformationJoinName, err := provider.GetPrimaryTableName(providerJoinTransformationID)
	if err != nil {
		return fmt.Errorf("invalid transformation table name: %v", err)
	}
	if joinTransformationTable.GetName() != transformationJoinName {
		return fmt.Errorf("Transformation table did not copy name")
	}
	numRows, err = joinTransformationTable.NumRows()
	if err != nil {
		return fmt.Errorf("Could not get num rows from transformation table")
	}
	if int(numRows) != len(testOfflineTableValues) {
		return fmt.Errorf("transformation table did not copy correct number of rows")
	}
	joinTransformationIterator, err := joinTransformationTable.IterateSegment(int64(len(testOfflineTableValues)))
	if err != nil {
		return err
	}
	i = 0
	for ; joinTransformationIterator.Next(); i++ {
		if joinTransformationIterator.Err() != nil {
			return err
		}
		joinTransformationTableRow := joinTransformationIterator.Values()
		values := reflect.ValueOf(testOfflineTableValues[i])
		for j := 0; j < values.NumField(); j++ {
			if joinTransformationTableRow[j] != values.Field(j).Interface() {
				return fmt.Errorf("Transformation table value does not match original value")
			}
		}
	}
	if i != len(testOfflineTableValues) {
		return fmt.Errorf("transformation table did not copy all rows")
	}

	return nil
}

func TestGetSourceMapping(t *testing.T) {
	templateString := "Some example text {{name1.variant1}} and more {{name2.variant2}}"
	replacements := map[string]string{"name1.variant1": "replacement1", "name2.variant2": "replacement2"}
	expectedSourceMap := []provider.SourceMapping{
		provider.SourceMapping{
			Template: "\"replacement1\"",
			Source:   "replacement1",
		},
		provider.SourceMapping{
			Template: "\"replacement2\"",
			Source:   "replacement2",
		},
	}

	sourceMap, err := getSourceMapping(templateString, replacements)
	if err != nil {
		t.Fatalf("Could not retrieve the source mapping: %v", err)
	}

	if !reflect.DeepEqual(sourceMap, expectedSourceMap) {
		t.Fatalf("source mapping did not generate the SourceMapping correctly. Expected %v, got %v", sourceMap, expectedSourceMap)
	}
}

func TestGetSourceMappingError(t *testing.T) {
	templateString := "Some example text {{name1.variant1}} and more {{name2.variant2}}"
	wrongReplacements := map[string]string{"name1.variant1": "replacement1", "name3.variant3": "replacement2"}
	_, err := getSourceMapping(templateString, wrongReplacements)
	if err == nil {
		t.Fatalf("getSourceMapping did not catch error: templateString {%v} and wrongReplacement {%v}", templateString, wrongReplacements)
	}
}

func TestGetOrderedSourceMappings(t *testing.T) {
	type testCase struct {
		name              string
		sources           []metadata.NameVariant
		sourceMap         map[string]string
		expectedSourceMap []provider.SourceMapping
		expectError       bool
	}

	testCases := []testCase{
		{
			name: "test ordered source mappings",
			sources: []metadata.NameVariant{
				{Name: "name1", Variant: "variant1"},
				{Name: "name2", Variant: "variant2"},
			},
			sourceMap: map[string]string{
				"name1.variant1": "tableA",
				"name2.variant2": "tableB",
			},
			expectedSourceMap: []provider.SourceMapping{
				{
					Template: "name1.variant1",
					Source:   "tableA",
				},
				{
					Template: "name2.variant2",
					Source:   "tableB",
				},
			},
			expectError: false,
		},
		{
			name: "test unordered source mappings",
			sources: []metadata.NameVariant{
				{Name: "name1", Variant: "variant1"},
				{Name: "name2", Variant: "variant2"},
				{Name: "name3", Variant: "variant3"},
				{Name: "name4", Variant: "variant4"},
			},
			sourceMap: map[string]string{
				"name2.variant2": "tableB",
				"name4.variant4": "tableD",
				"name1.variant1": "tableA",
				"name3.variant3": "tableC",
			},
			expectedSourceMap: []provider.SourceMapping{
				{
					Template: "name1.variant1",
					Source:   "tableA",
				},
				{
					Template: "name2.variant2",
					Source:   "tableB",
				},
				{
					Template: "name3.variant3",
					Source:   "tableC",
				},
				{
					Template: "name4.variant4",
					Source:   "tableD",
				},
			},
			expectError: false,
		},
		{
			name: "test missing key in source map",
			sources: []metadata.NameVariant{
				{Name: "name1", Variant: "variant1"},
				{Name: "name2", Variant: "variant2"},
			},
			sourceMap: map[string]string{
				"name1.variant1": "tableA",
			},
			expectedSourceMap: nil,
			expectError:       true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sourceMap, err := getOrderedSourceMappings(tc.sources, tc.sourceMap)
			if tc.expectError && err == nil {
				t.Fatalf("Expected error, but did not get one")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if !reflect.DeepEqual(sourceMap, tc.expectedSourceMap) {
				t.Fatalf("source mapping did not generate the SourceMapping correctly. Expected %v, got %v", sourceMap, tc.expectedSourceMap)
			}
		})
	}
}
