package kustoutils

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"io"
	"net/http"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/data/table"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	schemav1alpha1 "github.com/microsoft/azure-schema-operator/api/v1alpha1"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
)

// QueryClient interface is taken from https://github.com/Azure/azure-kusto-go/blob/master/kusto/ingest/query_client.go
// Replace with import once versions align.
// Note: This is a temporary solution to the issue of the Kusto client not aligned with v1 Azure SDK.
type QueryClient interface {
	io.Closer
	Auth() kusto.Authorization
	Endpoint() string
	Query(ctx context.Context, db string, query kusto.Stmt, options ...kusto.QueryOption) (*kusto.RowIterator, error)
	Mgmt(ctx context.Context, db string, query kusto.Stmt, options ...kusto.MgmtOption) (*kusto.RowIterator, error)
	HttpClient() *http.Client
}

// KustoCluster represents a kusto cluster
type KustoCluster struct {
	URI       string
	Databases []string
	Client    QueryClient
	// Client    *kusto.Client
	wrapper *Wrapper
}

// NewKustoCluster returns a new KustoCluster object with a client initialized
func NewKustoCluster(uri string) *KustoCluster {
	cls := &KustoCluster{
		URI:     uri,
		wrapper: NewDeltaWrapper(),
	}

	a, err := auth.NewAuthorizerFromEnvironmentWithResource(uri)
	if err != nil {
		log.Error().Err(err).Msgf("failed to authorize from env to %s", uri)
	}

	authorizer := kusto.Authorization{
		Authorizer: a,
		// Config: auth.NewClientCredentialsConfig(clientID, clientSecret, tenantID),
	}

	client, err := kusto.New(uri, authorizer)
	if err != nil {
		log.Error().Err(err).Msgf("failed to connect to %s", uri)
	}
	cls.Client = client
	return cls
}

// AquireTargets filters the DBs in the cluster and matchs them with the filter to return DBs to execute on.
func (c *KustoCluster) AquireTargets(filter schemav1alpha1.TargetFilter) (schemav1alpha1.ClusterTargets, error) {
	var targets schemav1alpha1.ClusterTargets
	var dbs []string
	var err error

	// b.1. get filtered list of dbs to execute on
	// TODO: Consider extracting this to the Cluster as a filter object
	if filter.DB != "" {
		dbs, err = c.ListDatabases(filter.DB)
	} else if len(filter.DBS) > 0 {
		// TODO: maybe change this to a filter instead of setting
		dbs = filter.DBS
	} else if filter.Webhook != "" {
		client := NewWebHookClient(nil)
		dbs, err = client.PerformQuery(filter.Webhook, ClusterNameFromURI(c.URI), filter.Label)
	} else {
		log.Info().Msg("Missing db filter - taking all dbs in the cluster")
		dbs, err = c.ListDatabases("")
	}
	if err != nil {
		log.Error().Err(err).Msg("failed retriving list of dbs from cluster")
		return targets, err
	}
	targets.DBs = dbs
	return targets, err
}

// ListDatabases lists kusto databases matching the regexp expression.
func (c *KustoCluster) ListDatabases(expression string) ([]string, error) {

	ctx := context.Background()
	nameFilter, err := regexp.Compile(expression)
	if err != nil {
		log.Error().Err(err).Msgf("parameter proveded is not a valid regexp: %s", expression)
		return nil, err
	}

	dbs := make([]string, 0)

	iter, err := c.Client.Mgmt(ctx, "", kusto.NewStmt(".show databases | project DatabaseName"))
	if err != nil {
		log.Error().Err(err).Msg("Failed to query mgmt api")
		return nil, err
	}
	defer iter.Stop()

	// .DoOnRowOrError() will call the function for every row in the table.
	err = iter.DoOnRowOrError(
		func(row *table.Row, inlineError *errors.Error) error {
			if row != nil {
				dbName := row.Values[0].String()
				if nameFilter.MatchString(dbName) {
					dbs = append(dbs, dbName)
					// log.Debug().Msgf("dbname passed filter: %s", dbName)
				}
			} else {
				// ignore inline errors - not relevant for this use case
				log.Error().Msgf("got inline error: %s", inlineError.Error())
			}
			// log.Debug().Msgf("dbname: %s", dbName)
			return nil
		},
	)
	if err != nil {
		log.Error().Err(err).Msg("failed to iterate results")
		return nil, err
	}
	return dbs, nil
}

// Execute runs the `ExecutionConfiguration` on the provided targets
func (c *KustoCluster) Execute(targets schemav1alpha1.ClusterTargets, config schemav1alpha1.ExecutionConfiguration) (schemav1alpha1.ClusterTargets, error) {
	done := schemav1alpha1.ClusterTargets{}
	err := RunDeltaKusto(config.JobFile)

	return done, err
}

// CreateExecConfiguration creates execution configuration for the given targets and `ConfigMap` configuration.
func (c *KustoCluster) CreateExecConfiguration(targets schemav1alpha1.ClusterTargets, cfgMap *v1.ConfigMap, failIfDataLoss bool) (schemav1alpha1.ExecutionConfiguration, error) {
	config := schemav1alpha1.ExecutionConfiguration{}
	kql, ok := cfgMap.Data["kql"]
	if !ok {
		return config, fmt.Errorf("no kql found in configmap")
	}
	kqlFile, err := StoreKQLSchemaToFile(kql)
	if err != nil {
		log.Error().Err(err).Msg("failed downloading kql to file")
		return config, err
	}
	deltaCfgFile, err := c.wrapper.CreateExecConfiguration(c.URI, targets.DBs, kqlFile, failIfDataLoss)
	if err != nil {
		log.Error().Err(err).Msg("failed generating delta kusto configuration file")
		return config, err
	}
	config.KQLFile = kqlFile
	config.JobFile = deltaCfgFile
	return config, nil
}

// // Difference returns the elements in `a` that aren't in `b`.
// func Difference(a, b []string) []string {
// 	mb := make(map[string]struct{}, len(b))
// 	for _, x := range b {
// 		mb[x] = struct{}{}
// 	}
// 	var diff []string
// 	for _, x := range a {
// 		if _, found := mb[x]; !found {
// 			diff = append(diff, x)
// 		}
// 	}
// 	return diff
// }

// ClusterNameFromURI returns the cluster name from the given URI
func ClusterNameFromURI(uri string) string {
	return strings.Split(strings.Split(uri, "https://")[1], ".")[0]
}
