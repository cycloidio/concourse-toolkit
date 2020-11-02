package main

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	atcDb "github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/db/encryption"
	"github.com/concourse/concourse/atc/db/lock"
	"github.com/concourse/flag"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//
// prometheus
//
type PrometheusConfig struct {
	BindIP   string `long:"prometheus-bind-ip" description:"IP to listen on to expose Prometheus metrics."`
	BindPort string `long:"prometheus-bind-port" description:"Port to listen on to expose Prometheus metrics."`
}

func (config *PrometheusConfig) bind() string {
	return fmt.Sprintf("%s:%s", config.BindIP, config.BindPort)
}

type PrometheusMetrics struct {
	builds             *prometheus.GaugeVec
	resources          *prometheus.GaugeVec
	runningTasks       *prometheus.GaugeVec
	orphanedContainers *prometheus.GaugeVec
	workers            *prometheus.GaugeVec
	workersActiveTasks *prometheus.GaugeVec
}

func NewMetrics() *PrometheusMetrics {

	builds := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "builds",
			Help:        "Get current build status for every jobs.",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"team",
			"pipeline",
			"job",
			"pipeline_paused",
			"start_time",
			"isManuallyTriggered",
			"isScheduled",
			"end_time",
			"status",
			"name",
		},
	)

	prometheus.MustRegister(builds)

	resources := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "resources",
			Help:        "Get current resources status for each pipelines.",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"team",
			"pipeline",
			"pipeline_paused",
			"type",
			"failing_to_check",
			"checkEvery",
			"name",
		},
	)
	prometheus.MustRegister(resources)

	// Compact set
	// buildsFailed.WithLabelValues("bob", "put").Set(4)
	// Set with named labels
	// buildsFailed.With(prometheus.Labels{"type": "delete", "user": "alice"}).Set(2)

	runningTasks := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "running_tasks",
			Help:        "Get current running tasks.",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"team",
			"pipeline",
			"job",
			"start_time",
			"step",
			"worker",
			"isManuallyTriggered",
			"isScheduled",
			"build_status",
			"build",
			"type",
		},
	)
	prometheus.MustRegister(runningTasks)

	orphanedContainers := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "orphaned_containers",
			Help:        ".",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"team",
			"pipeline",
			"job",
			"worker",
			"type",
			"status",
		},
	)
	prometheus.MustRegister(orphanedContainers)

	workers := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "workers",
			Help:        ".",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"name",
			"version",
			"state",
			"team",
			"ephemeral",
			"start_time",
			"platform",
			"tags",
		},
	)
	prometheus.MustRegister(workers)

	workersActiveTasks := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "concourse_toolkit",
			// Subsystem:   "builds",
			Name:        "workers_active_tasks",
			Help:        ".",
			ConstLabels: prometheus.Labels{"exporter": "concourse-toolkit"},
		},
		[]string{
			"name",
			"version",
			"state",
			"team",
			"ephemeral",
			"start_time",
			"platform",
			"tags",
		},
	)
	prometheus.MustRegister(workersActiveTasks)

	return &PrometheusMetrics{
		builds:             builds,
		resources:          resources,
		runningTasks:       runningTasks,
		orphanedContainers: orphanedContainers,
		workers:            workers,
		workersActiveTasks: workersActiveTasks,
	}
}

//
// Concourse Database connect
//
type connectionRetryingDriver struct {
	driver.Driver
	driverName string
}

func connectDb() (atcDb.Conn, lock.LockFactory, error) {
	var retryingDriverName = "too-many-connections-retrying"

	psqlConfig := flag.PostgresConfig{Host: viper.GetString("host"), Port: uint16(viper.GetInt("port")), User: viper.GetString("user"), Password: viper.GetString("password"), Database: viper.GetString("database"), SSLMode: "disable"}
	fmt.Printf("DEBUG : %s\n", psqlConfig.ConnectionString())
	var driverName = "postgres"
	var maxConn = 15
	// Mix from atccmd.constructDBConn adn db.Open
	// Copy paste of db struct from https://github.com/concourse/atc/blob/master/db/open.go#L318
	// We previously used constructDBConn from https://github.com/concourse/atc/blob/master/atccmd/command.go#L905 which called Open from https://github.com/concourse/atc/blob/master/db/open.go#L57
	//But we moved aways because this methode force a db migrate on concourse side. We don't want that.
	SetupConnectionRetryingDriver(
		"postgres",
		psqlConfig.ConnectionString(),
		retryingDriverName,
	)

	lockConn, err := sql.Open(driverName, psqlConfig.ConnectionString())
	if err != nil {
		return nil, nil, err
	}
	lockConn.SetMaxOpenConns(1)
	lockConn.SetMaxIdleConns(1)
	lockConn.SetConnMaxLifetime(0)

	// lockFactory := lock.NewLockFactory(lockConn, metric.LogLockAcquired, metric.LogLockReleased)
	lockFactory := lock.NewLockFactory(lockConn, nil, nil)

	sqlDb, err := sql.Open(driverName, psqlConfig.ConnectionString())
	if err != nil {
		return nil, nil, err
	}

	strategy := encryption.NewNoEncryption()
	if viper.GetString("encryption-key") != "" {
		fmt.Printf("DEBUG : Using a DB encryption key\n")
		var cipher flag.Cipher
		if err := cipher.UnmarshalFlag(viper.GetString("encryption-key")); err != nil {
			return nil, nil, err
		}
		strategy = encryption.NewKey(cipher.AEAD)
	}

	listener := pq.NewDialListener(keepAliveDialer{}, psqlConfig.ConnectionString(), time.Second, time.Minute, nil)
	dbConn := &db{
		DB:         sqlDb,
		bus:        atcDb.NewNotificationsBus(listener, sqlDb),
		encryption: strategy,
		name:       "concourse-toolkit",
	}
	dbConn.SetMaxOpenConns(maxConn)
	dbConn.SetMaxIdleConns(maxConn / 2)

	return dbConn, lockFactory, nil
}

//
// Copy paste of private concourse structures to be able to provide the expected dbConn expected format. (used in connectDb)
//

// From atc/db/connection_retrying_driver.go
func SetupConnectionRetryingDriver(
	delegateDriverName string,
	sqlDataSource string,
	newDriverName string,
) {
	for _, driverName := range sql.Drivers() {
		if driverName == newDriverName {
			return
		}
	}
	delegateDBConn, err := sql.Open(delegateDriverName, sqlDataSource)
	if err == nil {
		// ignoring any connection errors since we only need this to access the driver struct
		_ = delegateDBConn.Close()
	}

	connectionRetryingDriver := &connectionRetryingDriver{
		delegateDBConn.Driver(),
		delegateDriverName,
	}
	sql.Register(newDriverName, connectionRetryingDriver)
}

// concourse/atc/db/keepalive_dialer.go
type keepAliveDialer struct {
}

func (d keepAliveDialer) Dial(network, address string) (net.Conn, error) {
	dialer := &net.Dialer{
		KeepAlive: 15 * time.Second,
	}

	return dialer.Dial(network, address)
}

func (d keepAliveDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{
		KeepAlive: 15 * time.Second,
		Timeout:   timeout,
	}

	return dialer.Dial(network, address)
}

// Those are just copy paste adapting some path cause of includes : eg NotificationsBus -> atcDb.NotificationsBus

type db struct {
	*sql.DB

	bus        atcDb.NotificationsBus
	encryption encryption.Strategy
	name       string
}

func (db *db) Name() string {
	return db.name
}

func (db *db) Bus() atcDb.NotificationsBus {
	return db.bus
}

func (db *db) EncryptionStrategy() encryption.Strategy {
	return db.encryption
}

func (db *db) Close() error {
	var errs error
	dbErr := db.DB.Close()
	if dbErr != nil {
		errs = multierror.Append(errs, dbErr)
	}

	busErr := db.bus.Close()
	if busErr != nil {
		errs = multierror.Append(errs, busErr)
	}

	return errs
}

// Close ignores errors, and should used with defer.
// makes errcheck happy that those errs are captured
func Close(c io.Closer) {
	_ = c.Close()
}

func (db *db) Begin() (atcDb.Tx, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}

	return &dbTx{tx, atcDb.GlobalConnectionTracker.Track()}, nil
}

func (db *db) Exec(query string, args ...interface{}) (sql.Result, error) {
	defer atcDb.GlobalConnectionTracker.Track().Release()
	return db.DB.Exec(query, args...)
}

func (db *db) Prepare(query string) (*sql.Stmt, error) {
	defer atcDb.GlobalConnectionTracker.Track().Release()
	return db.DB.Prepare(query)
}

func (db *db) Query(query string, args ...interface{}) (*sql.Rows, error) {
	defer atcDb.GlobalConnectionTracker.Track().Release()
	return db.DB.Query(query, args...)
}

// to conform to squirrel.Runner interface
func (db *db) QueryRow(query string, args ...interface{}) squirrel.RowScanner {
	defer atcDb.GlobalConnectionTracker.Track().Release()
	return db.DB.QueryRow(query, args...)
}

type dbTx struct {
	*sql.Tx

	session *atcDb.ConnectionSession
}

// to conform to squirrel.Runner interface
func (tx *dbTx) QueryRow(query string, args ...interface{}) squirrel.RowScanner {
	return tx.Tx.QueryRow(query, args...)
}

func (tx *dbTx) Commit() error {
	defer tx.session.Release()
	return tx.Tx.Commit()
}

func (tx *dbTx) Rollback() error {
	defer tx.session.Release()
	return tx.Tx.Rollback()
}

// Rollback ignores errors, and should be used with defer.
// makes errcheck happy that those errs are captured
func Rollback(tx atcDb.Tx) {
	_ = tx.Rollback()
}

type nonOneRowAffectedError struct {
	RowsAffected int64
}

func (err nonOneRowAffectedError) Error() string {
	return fmt.Sprintf("expected 1 row to be updated; got %d", err.RowsAffected)
}

//
// Concourse toolkit
//

func metricOrphanedContainers(promMetrics *PrometheusMetrics, dbContainerRepository atcDb.ContainerRepository, dbBuildFactory atcDb.BuildFactory) {

	creatingContainer, createdContainer, destroyingContainer, err := dbContainerRepository.FindOrphanedContainers()
	if err != nil {
		fmt.Println("dbContainerRepository.FindOrphanedContainers:\n", err.Error())
	}
	var defaultTeam = ""

	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.orphanedContainers.Reset()

	for _, container := range creatingContainer {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {
			build, _, err := dbBuildFactory.Build(container.Metadata().BuildID)
			if err != nil {
				fmt.Println("dbBuildFactory.Build:\n", err.Error())
			}
			if build != nil {
				team = build.TeamName()
			}
		}
		promMetrics.orphanedContainers.With(prometheus.Labels{
			"team":     team,
			"pipeline": container.Metadata().PipelineName,
			"job":      container.Metadata().JobName,
			"worker":   container.WorkerName(),
			"type":     string(container.Metadata().Type),
			"status":   "creating",
		}).Inc()

	}

	for _, container := range createdContainer {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {
			build, _, err := dbBuildFactory.Build(container.Metadata().BuildID)
			if err != nil {
				fmt.Println("dbBuildFactory.Build:\n", err.Error())
			}
			if build != nil {
				team = build.TeamName()
			}
		}
		promMetrics.orphanedContainers.With(prometheus.Labels{
			"team":     team,
			"pipeline": container.Metadata().PipelineName,
			"job":      container.Metadata().JobName,
			"worker":   container.WorkerName(),
			"type":     string(container.Metadata().Type),
			"status":   "created",
		}).Inc()
		// build.Status() ?

	}

	for _, container := range destroyingContainer {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {

			build, _, err := dbBuildFactory.Build(container.Metadata().BuildID)
			if err != nil {
				fmt.Println("dbBuildFactory.Build:\n", err.Error())
			}
			if build != nil {
				team = build.TeamName()
			}
		}
		promMetrics.orphanedContainers.With(prometheus.Labels{
			"team":     team,
			"pipeline": container.Metadata().PipelineName,
			"job":      container.Metadata().JobName,
			"worker":   container.WorkerName(),
			"type":     string(container.Metadata().Type),
			"status":   "destroying",
		}).Add(1)
	}

}

func metricRunningTasks(promMetrics *PrometheusMetrics, dbBuildFactory atcDb.BuildFactory, teamFactory atcDb.TeamFactory) {

	builds, err := dbBuildFactory.GetAllStartedBuilds()
	if err != nil {
		fmt.Println("dbBuildFactory.GetAllStartedBuilds:\n", err.Error())
	}
	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.runningTasks.Reset()

	for _, build := range builds {
		team := teamFactory.GetByID(build.TeamID())
		containers, err := team.FindContainersByMetadata(atcDb.ContainerMetadata{PipelineID: build.PipelineID(), JobID: build.JobID(), BuildID: build.ID()})
		if err != nil {
			fmt.Println("team.FindContainersByMetadata:\n", err.Error())
		}
		for _, container := range containers {
			promMetrics.runningTasks.With(prometheus.Labels{
				"team":                build.TeamName(),
				"pipeline":            build.PipelineName(),
				"job":                 build.JobName(),
				"start_time":          build.StartTime().String(),
				"build":               build.Name(),
				"isManuallyTriggered": strconv.FormatBool(build.IsManuallyTriggered()),
				"isScheduled":         strconv.FormatBool(build.IsScheduled()),
				"worker":              container.WorkerName(),
				"build_status":        string(build.Status()),
				"step":                container.Metadata().StepName,
				"type":                string(container.Metadata().Type),
			}).Set(1)
		}
	}
	return
}

func metricBuildsAndResources(promMetrics *PrometheusMetrics, teamFactory atcDb.TeamFactory) {

	teams, err := teamFactory.GetTeams()
	if err != nil {
		fmt.Println("atcDb.NewTeamFactory:\n", err.Error())
	}
	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.resources.Reset()
	promMetrics.builds.Reset()

	for _, team := range teams {

		pipelines, err := team.Pipelines()
		if err != nil {
			fmt.Println("team.Pipelines:\n", err.Error())
		}
		for _, pipeline := range pipelines {

			resources, err := pipeline.Resources()
			if err != nil {
				fmt.Println("pipeline.Resources:\n", err.Error())
				continue
			}
			for _, resource := range resources {
				failingToCheck := float64(0)
				if resource.CheckError != nil {
					failingToCheck = 1
				}
				promMetrics.resources.With(prometheus.Labels{
					"team":             pipeline.TeamName(),
					"pipeline":         resource.PipelineName(),
					"pipeline_paused":  strconv.FormatBool(pipeline.Paused()),
					"type":             resource.Type(),
					"checkEvery":       resource.CheckEvery(),
					"failing_to_check": strconv.FormatFloat(failingToCheck, 'f', 6, 64),
					"name":             resource.Name(),
				}).Set(failingToCheck)
			}

			jobs, err := pipeline.Jobs()
			if err != nil {
				fmt.Println(err)
				continue
			}
			for _, job := range jobs {

				build, nextBuild, err := job.FinishedAndNextBuild()
				if err != nil {
					fmt.Println(err)
					continue
				}

				// Get running builds
				if nextBuild != nil {
					// fmt.Println("  -  NextBuild (Running)", job.Name(), nextBuild.Name(), nextBuild.Status())
					floatBuildName, _ := strconv.ParseFloat(nextBuild.Name(), 64)
					promMetrics.builds.With(prometheus.Labels{
						"team":                job.TeamName(),
						"pipeline":            job.PipelineName(),
						"pipeline_paused":     strconv.FormatBool(pipeline.Paused()),
						"isManuallyTriggered": strconv.FormatBool(nextBuild.IsManuallyTriggered()),
						"isScheduled":         strconv.FormatBool(nextBuild.IsScheduled()),
						"job":                 job.Name(),
						"status":              string(nextBuild.Status()),
						"start_time":          nextBuild.StartTime().String(),
						"end_time":            nextBuild.EndTime().String(),
						"name":                nextBuild.Name(),
					}).Set(floatBuildName)
				}

				// Get finished builds
				if build != nil {
					// fmt.Println("  - ", job.Name(), build.Name(), build.Status())
					floatBuildName, _ := strconv.ParseFloat(build.Name(), 64)
					promMetrics.builds.With(prometheus.Labels{
						"team":                job.TeamName(),
						"pipeline":            job.PipelineName(),
						"pipeline_paused":     strconv.FormatBool(pipeline.Paused()),
						"job":                 job.Name(),
						"status":              string(build.Status()),
						"isManuallyTriggered": strconv.FormatBool(build.IsManuallyTriggered()),
						"isScheduled":         strconv.FormatBool(build.IsScheduled()),
						"start_time":          build.StartTime().String(),
						"end_time":            build.EndTime().String(),
						"name":                build.Name(),
					}).Set(floatBuildName)

				}

				// Get pending builds
				pendingBuilds, err := job.GetPendingBuilds()
				if err != nil {
					fmt.Println("job.GetPendingBuilds:\n", err.Error())
				}
				for _, pendingBuild := range pendingBuilds {
					if pendingBuild != nil {
						// No last build status
						// fmt.Println("  -  Pending ", job.Name(), pendingBuild.Name(), pendingBuild.Status())
						floatBuildName, _ := strconv.ParseFloat(pendingBuild.Name(), 64)
						promMetrics.builds.With(prometheus.Labels{
							"team":                job.TeamName(),
							"pipeline":            job.PipelineName(),
							"pipeline_paused":     strconv.FormatBool(pipeline.Paused()),
							"job":                 job.Name(),
							"status":              string(pendingBuild.Status()),
							"isManuallyTriggered": strconv.FormatBool(pendingBuild.IsManuallyTriggered()),
							"isScheduled":         strconv.FormatBool(pendingBuild.IsScheduled()),
							"start_time":          pendingBuild.StartTime().String(),
							"end_time":            pendingBuild.EndTime().String(),
							"name":                pendingBuild.Name(),
						}).Set(floatBuildName)

					}
				}
			}
		}
	}
}

func metricWorkers(promMetrics *PrometheusMetrics, dbWorkerFactory atcDb.WorkerFactory) {

	workers, err := dbWorkerFactory.Workers()
	if err != nil {
		fmt.Println("dbWorkerFactory.Workers:\n", err.Error())
	}
	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.workers.Reset()

	for _, worker := range workers {
		promMetrics.workers.With(prometheus.Labels{
			"name":       worker.Name(),
			"version":    *worker.Version(),
			"state":      string(worker.State()),
			"team":       worker.TeamName(),
			"ephemeral":  strconv.FormatBool(worker.Ephemeral()),
			"start_time": worker.StartTime().String(),
			"platform":   worker.Platform(),
			"tags":       strings.Join(worker.Tags(), ","),
		}).Set(float64(worker.ActiveContainers()))

		activeTasks, _ := worker.ActiveTasks()
		promMetrics.workersActiveTasks.With(prometheus.Labels{
			"name":       worker.Name(),
			"version":    *worker.Version(),
			"state":      string(worker.State()),
			"team":       worker.TeamName(),
			"ephemeral":  strconv.FormatBool(worker.Ephemeral()),
			"start_time": worker.StartTime().String(),
			"platform":   worker.Platform(),
			"tags":       strings.Join(worker.Tags(), ","),
		}).Set(float64(activeTasks))
	}
}

func getSomeMetrics(promMetrics *PrometheusMetrics, dbWorkerFactory atcDb.WorkerFactory, teamFactory atcDb.TeamFactory, dbContainerRepository atcDb.ContainerRepository, dbBuildFactory atcDb.BuildFactory) {

	metricWorkers(promMetrics, dbWorkerFactory)
	metricBuildsAndResources(promMetrics, teamFactory)
	metricRunningTasks(promMetrics, dbBuildFactory, teamFactory)
	metricOrphanedContainers(promMetrics, dbContainerRepository, dbBuildFactory)
}

func run(cmd *cobra.Command, args []string) {
	fmt.Printf("DEBUG : Exposing metrics on http://0.0.0.0:%s/metrics\n", viper.GetString("metrics-port"))
	dbConn, lockFactory, _ := connectDb()
	err := dbConn.Ping()
	if err != nil {
		fmt.Println("Error %s\n", err.Error())
		return
	}

	config := &PrometheusConfig{BindIP: "0.0.0.0", BindPort: viper.GetString("metrics-port")}
	listener, err := net.Listen("tcp", config.bind())
	if err != nil {
		fmt.Println(err)
	}

	// go http.Serve(listener, promhttp.Handler())
	go http.Serve(listener, promhttp.Handler())

	promMetrics := NewMetrics()

	dbWorkerFactory := atcDb.NewWorkerFactory(dbConn)
	teamFactory := atcDb.NewTeamFactory(dbConn, lockFactory)
	dbContainerRepository := atcDb.NewContainerRepository(dbConn)
	dbBuildFactory := atcDb.NewBuildFactory(dbConn, lockFactory, 5*time.Minute)

	for {
		getSomeMetrics(promMetrics, dbWorkerFactory, teamFactory, dbContainerRepository, dbBuildFactory)
		time.Sleep(60 * time.Second)
	}
}

func main() {

	viper.AutomaticEnv()
	viper.SetEnvPrefix("CONCOURSE_TOOLKIT")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	// // memory debug pprof
	// go func() {
	// 	log.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	var rootCmd = &cobra.Command{
		Short: "Concourse toolkit metrics",
		Long: `Concourse toolkit is here to debug and expose concourse internal metrics.\n
Each option could be used in uppercase as envvar prefixed by CONCOURSE_TOOLKIT. Eg CONCOURSE_TOOLKIT_HOST.`,
		Run:              run,
		TraverseChildren: true}

	// Can use env var like CONCOURSE_TOOLKIT=127.0.0.1

	// prometheus metrics port
	rootCmd.Flags().StringP("metrics-port", "", "9100", "Port on which expose prometheus metrics.")
	viper.BindPFlag("metrics-port", rootCmd.Flags().Lookup("metrics-port"))

	// PGSQL host
	rootCmd.Flags().StringP("host", "H", "127.0.0.1", "PGSQL host")
	viper.BindPFlag("host", rootCmd.Flags().Lookup("host"))

	// PGSQL user
	rootCmd.Flags().StringP("user", "u", "super", "PGSQL user")
	viper.BindPFlag("user", rootCmd.Flags().Lookup("user"))

	// PGSQL password
	rootCmd.Flags().StringP("password", "P", "concourse", "PGSQL password")
	viper.BindPFlag("password", rootCmd.Flags().Lookup("password"))

	// PGSQL database
	rootCmd.Flags().StringP("database", "d", "concourse", "PGSQL database")
	viper.BindPFlag("database", rootCmd.Flags().Lookup("database"))

	// PGSQL port
	rootCmd.Flags().IntP("port", "p", 5432, "PGSQL port")
	viper.BindPFlag("port", rootCmd.Flags().Lookup("port"))

	// concourse db encryption key
	rootCmd.Flags().StringP("encryption-key", "k", "", "Concourse DB encryption key")
	viper.BindPFlag("encryption-key", rootCmd.Flags().Lookup("encryption-key"))

	// rootCmd.MarkFlagRequired("host")
	rootCmd.Execute()

}
