package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/concourse/atc/atccmd"
	atcDb "github.com/concourse/atc/db"
	"github.com/concourse/atc/db/encryption"
	"github.com/concourse/atc/db/lock"
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
			"start_time",
			"platform",
			"tags",
		},
	)
	prometheus.MustRegister(workers)

	return &PrometheusMetrics{
		builds:             builds,
		resources:          resources,
		runningTasks:       runningTasks,
		orphanedContainers: orphanedContainers,
		workers:            workers,
	}
}

//
// Concourse Database connect
//

func connectDb() (atcDb.Conn, lock.LockFactory, error) {

	psqlConfig := atccmd.PostgresConfig{Host: viper.GetString("host"), Port: uint16(viper.GetInt("port")), User: viper.GetString("user"), Password: viper.GetString("password"), Database: viper.GetString("database"), SSLMode: "disable"}
	fmt.Printf("DEBUG : %s\n", psqlConfig.ConnectionString())
	var driverName = "postgres"

	// Mix from atccmd.constructDBConn adn db.Open
	// Copy paste of db struct from https://github.com/concourse/atc/blob/master/db/open.go#L318
	// We previously used constructDBConn from https://github.com/concourse/atc/blob/master/atccmd/command.go#L905 which called Open from https://github.com/concourse/atc/blob/master/db/open.go#L57
	//But we moved aways because this methode force a db migrate on concourse side. We don't want that.
	sqlDb, err := sql.Open(driverName, psqlConfig.ConnectionString())
	if err != nil {
		return nil, nil, err
	}
	sqlDb.SetMaxOpenConns(32)

	listener := pq.NewListener(psqlConfig.ConnectionString(), time.Second, time.Minute, nil)
	var strategy encryption.Strategy = encryption.NewNoEncryption()

	dbConn := &db{
		DB:         sqlDb,
		bus:        atcDb.NewNotificationsBus(listener, sqlDb),
		encryption: strategy,
		name:       "concourse-toolkit",
	}

	// atccmd.constructLockConn Copied private methodes constructLockConn from https://github.com/concourse/atc/blob/master/atccmd/command.go#L907
	lockConn, err := sql.Open(driverName, psqlConfig.ConnectionString())
	if err != nil {
		return nil, nil, err
	}
	lockConn.SetMaxOpenConns(10)
	lockConn.SetMaxIdleConns(2)
	lockConn.SetConnMaxLifetime(0)
	lockFactory := lock.NewLockFactory(lockConn)

	return dbConn, lockFactory, nil
}

//
// Copy paste of private concourse structures to be able to provide the expected dbConn expected format. (used in connectDb)
//

// Those are just copy paste adapting some path cause of includes : eg NotificationsBus -> atcDb.NotificationsBus
type db struct {
	*sql.DB

	bus        atcDb.NotificationsBus
	encryption encryption.Strategy
	name       string
}

func (db *db) Begin() (atcDb.Tx, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}

	return &dbTx{tx, atcDb.GlobalConnectionTracker.Track()}, nil
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
func (db *db) QueryRow(query string, args ...interface{}) squirrel.RowScanner {
	defer atcDb.GlobalConnectionTracker.Track().Release()
	return db.DB.QueryRow(query, args...)
}

type dbTx struct {
	*sql.Tx

	session *atcDb.ConnectionSession
}

func (tx *dbTx) QueryRow(query string, args ...interface{}) squirrel.RowScanner {
	return tx.Tx.QueryRow(query, args...)
}

//
// Concourse toolkit
//

func metricOrphanedContainers(promMetrics *PrometheusMetrics, dbConn atcDb.Conn, lockFactory lock.LockFactory) {
	dbContainerRepository := atcDb.NewContainerRepository(dbConn)
	dbBuildFactory := atcDb.NewBuildFactory(dbConn, lockFactory)
	failedContainers, err := dbContainerRepository.FindFailedContainers()
	if err != nil {
		fmt.Println("dbContainerRepository.FindFailedContainers: %s\n", err.Error())
	}
	creatingContainer, createdContainer, destroyingContainer, err := dbContainerRepository.FindOrphanedContainers()
	if err != nil {
		fmt.Println("dbContainerRepository.FindOrphanedContainers: %s\n", err.Error())
	}
	var defaultTeam = ""

	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.orphanedContainers.Reset()

	for _, container := range failedContainers {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {
			build, _, err := dbBuildFactory.Build(container.Metadata().BuildID)
			if err != nil {
				fmt.Println("dbBuildFactory.Build: %s\n", err.Error())
			}
			team = build.TeamName()
		}
		promMetrics.orphanedContainers.With(prometheus.Labels{
			"team":     team,
			"pipeline": container.Metadata().PipelineName,
			"job":      container.Metadata().JobName,
			"worker":   container.WorkerName(),
			"type":     string(container.Metadata().Type),
			"status":   "failed",
		}).Inc()

	}

	for _, container := range creatingContainer {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {
			build, _, err := dbBuildFactory.Build(container.Metadata().BuildID)
			if err != nil {
				fmt.Println("dbBuildFactory.Build: %s\n", err.Error())
			}
			team = build.TeamName()
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
				fmt.Println("dbBuildFactory.Build: %s\n", err.Error())
			}
			team = build.TeamName()
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
				fmt.Println("dbBuildFactory.Build: %s\n", err.Error())
			}
			team = build.TeamName()
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

func metricRunningTasks(promMetrics *PrometheusMetrics, dbConn atcDb.Conn, lockFactory lock.LockFactory) {

	dbBuildFactory := atcDb.NewBuildFactory(dbConn, lockFactory)
	teamFactory := atcDb.NewTeamFactory(dbConn, lockFactory)

	builds, err := dbBuildFactory.GetAllStartedBuilds()
	if err != nil {
		fmt.Println("dbBuildFactory.GetAllStartedBuilds: %s\n", err.Error())
	}
	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.runningTasks.Reset()

	for _, build := range builds {
		team := teamFactory.GetByID(build.TeamID())
		containers, err := team.FindContainersByMetadata(atcDb.ContainerMetadata{PipelineID: build.PipelineID(), JobID: build.JobID(), BuildID: build.ID()})
		if err != nil {
			fmt.Println("team.FindContainersByMetadata: %s\n", err.Error())
		}
		for _, container := range containers {
			promMetrics.runningTasks.With(prometheus.Labels{
				"team":         build.TeamName(),
				"pipeline":     build.PipelineName(),
				"job":          build.JobName(),
				"start_time":   build.StartTime().String(),
				"build":        build.Name(),
				"worker":       container.WorkerName(),
				"build_status": string(build.Status()),
				"step":         container.Metadata().StepName,
				"type":         string(container.Metadata().Type),
			}).Set(1)
		}
	}
	return
}

func metricBuildsAndResources(promMetrics *PrometheusMetrics, dbConn atcDb.Conn, lockFactory lock.LockFactory) {

	teamFactory := atcDb.NewTeamFactory(dbConn, lockFactory)
	teams, err := teamFactory.GetTeams()
	if err != nil {
		fmt.Println("atcDb.NewTeamFactory: %s\n", err.Error())
	}
	// As our metrics are a relative status of current state. Reset all old metrics declared during
	// the previous iteration
	promMetrics.resources.Reset()
	promMetrics.builds.Reset()

	for _, team := range teams {

		pipelines, err := team.Pipelines()
		if err != nil {
			fmt.Println("team.Pipelines: %s\n", err.Error())
		}
		for _, pipeline := range pipelines {

			resources, err := pipeline.Resources()
			if err != nil {
				fmt.Println("pipeline.Resources: %s\n", err.Error())
				continue
			}
			for _, resource := range resources {
				failingToCheck := float64(0)
				if resource.FailingToCheck() {
					failingToCheck = 1
				}
				promMetrics.resources.With(prometheus.Labels{
					"team":             pipeline.TeamName(),
					"pipeline":         resource.PipelineName(),
					"pipeline_paused":  strconv.FormatBool(pipeline.Paused()),
					"type":             resource.Type(),
					"failing_to_check": strconv.FormatBool(resource.FailingToCheck()),
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
						"team":            job.TeamName(),
						"pipeline":        job.PipelineName(),
						"pipeline_paused": strconv.FormatBool(pipeline.Paused()),
						"job":             job.Name(),
						"status":          string(nextBuild.Status()),
						"start_time":      nextBuild.StartTime().String(),
						"end_time":        nextBuild.EndTime().String(),
						"name":            nextBuild.Name(),
					}).Set(floatBuildName)
				}

				// Get finished builds
				if build != nil {
					// fmt.Println("  - ", job.Name(), build.Name(), build.Status())
					floatBuildName, _ := strconv.ParseFloat(build.Name(), 64)
					promMetrics.builds.With(prometheus.Labels{
						"team":            job.TeamName(),
						"pipeline":        job.PipelineName(),
						"pipeline_paused": strconv.FormatBool(pipeline.Paused()),
						"job":             job.Name(),
						"status":          string(build.Status()),
						"start_time":      build.StartTime().String(),
						"end_time":        build.EndTime().String(),
						"name":            build.Name(),
					}).Set(floatBuildName)

				}

				// Get pending builds
				pendingBuilds, err := job.GetPendingBuilds()
				if err != nil {
					fmt.Println("job.GetPendingBuilds: %s\n", err.Error())
				}
				for _, pendingBuild := range pendingBuilds {
					if pendingBuild != nil {
						// No last build status
						// fmt.Println("  -  Pending ", job.Name(), pendingBuild.Name(), pendingBuild.Status())
						floatBuildName, _ := strconv.ParseFloat(pendingBuild.Name(), 64)
						promMetrics.builds.With(prometheus.Labels{
							"team":            job.TeamName(),
							"pipeline":        job.PipelineName(),
							"pipeline_paused": strconv.FormatBool(pipeline.Paused()),
							"job":             job.Name(),
							"status":          string(pendingBuild.Status()),
							"start_time":      pendingBuild.StartTime().String(),
							"end_time":        pendingBuild.EndTime().String(),
							"name":            pendingBuild.Name(),
						}).Set(floatBuildName)

					}
				}
			}
		}
	}
}

func metricWorkers(promMetrics *PrometheusMetrics, dbConn atcDb.Conn) {

	dbWorkerFactory := atcDb.NewWorkerFactory(dbConn)
	workers, err := dbWorkerFactory.Workers()
	if err != nil {
		fmt.Println("dbWorkerFactory.Workers: %s\n", err.Error())
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
			"start_time": strconv.FormatInt(worker.StartTime(), 10),
			"platform":   worker.Platform(),
			"tags":       strings.Join(worker.Tags(), ","),
		}).Set(float64(worker.ActiveContainers()))

	}
}

func getSomeMetrics(promMetrics *PrometheusMetrics, dbConn atcDb.Conn, lockFactory lock.LockFactory) {
	metricWorkers(promMetrics, dbConn)
	metricBuildsAndResources(promMetrics, dbConn, lockFactory)
	metricRunningTasks(promMetrics, dbConn, lockFactory)
	metricOrphanedContainers(promMetrics, dbConn, lockFactory)
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

	for {
		getSomeMetrics(promMetrics, dbConn, lockFactory)
		time.Sleep(60 * time.Second)
	}
}

func main() {

	viper.AutomaticEnv()
	viper.SetEnvPrefix("CONCOURSE_TOOLKIT")

	var rootCmd = &cobra.Command{
		Short: "Concourse toolkit metrics",
		Long: `Concourse toolkit is here to debug and expose concourse internal metrics.\n
Each option could be used in uppercase as envvar prefixed by CONCOURSE_TOOLKIT. Eg CONCOURSE_TOOLKIT_HOST.`,
		Run: run,

		// Run: func(cmd *cobra.Command, args []string) {
		// },
		TraverseChildren: true}

	// Can use env var like CONCOURSE_TOOLKIT=127.0.0.1

	// metrics-port
	rootCmd.Flags().StringP("metrics-port", "", "9100", "Port on which expose prometheus metrics.")
	viper.BindPFlag("metrics-port", rootCmd.Flags().Lookup("metrics-port"))

	// psql host
	rootCmd.Flags().StringP("host", "H", "127.0.0.1", "Psql host")
	viper.BindPFlag("host", rootCmd.Flags().Lookup("host"))

	// psql user
	rootCmd.Flags().StringP("user", "u", "super", "Psql user")
	viper.BindPFlag("user", rootCmd.Flags().Lookup("user"))

	// psql user
	rootCmd.Flags().StringP("password", "P", "concourse", "Psql password")
	viper.BindPFlag("password", rootCmd.Flags().Lookup("password"))

	// psql user
	rootCmd.Flags().StringP("database", "d", "concourse", "Psql database")
	viper.BindPFlag("database", rootCmd.Flags().Lookup("database"))

	// psql user
	rootCmd.Flags().IntP("port", "p", 5432, "Psql port")
	viper.BindPFlag("port", rootCmd.Flags().Lookup("port"))

	// rootCmd.MarkFlagRequired("host")
	rootCmd.Execute()

}

// Dump of code :

// func ShowTeams(dbConn atcDb.Conn, lockFactory lock.LockFactory) {
// 	teamFactory := atcDb.NewTeamFactory(dbConn, lockFactory)
// 	teams, _ := teamFactory.GetTeams()
//
// 	for _, team := range teams {
// 		//debug filter
// 		if team.Name() != "pages-jaunes" {
// 			continue
// 		}
//
// 		fmt.Println("### Team : ", team.Name())
//
// 		// Workers for a team
// 		// fmt.Println("# Workers : ")
// 		// workers, _ := team.Workers()
// 		// for _, worker := range workers {
// 		// 	fmt.Println(worker.Name(), worker.State())
// 		// }
//
// 		fmt.Println("# Pipelines : ")
// 		pipelines, _ := team.Pipelines()
// 		for _, pipeline := range pipelines {
//
// 			fmt.Println(" * ", pipeline.Name())
// 			jobs, err := pipeline.Jobs()
// 			if err != nil {
// 				fmt.Println(err)
// 				continue
// 			}
// 			for _, job := range jobs {
// 				build, nextBuild, err := job.FinishedAndNextBuild()
// 				if err != nil {
// 					fmt.Println(err)
// 					continue
// 				}
//
// 				// This seems also to show running jobs
// 				if nextBuild != nil {
// 					// No last build status
// 					fmt.Println("  -  NextBuild (Running)", job.Name(), nextBuild.Name(), nextBuild.Status())
// 					containers, _ := team.FindContainersByMetadata(atcDb.ContainerMetadata{PipelineID: pipeline.ID(), JobID: job.ID(), BuildID: nextBuild.ID()})
// 					for _, container := range containers {
// 						fmt.Println(container.ID(), container.WorkerName(), container.Metadata())
// 					}
//
// 				}
//
// 				if build != nil {
// 					// if (build != nil) && (build.Status() != "succeeded") {
// 					fmt.Println("  - ", job.Name(), build.Name(), build.Status())
//
// 				}
//
// 				pendingBuilds, err := job.GetPendingBuilds()
// 				for _, pendingBuild := range pendingBuilds {
// 					if pendingBuild != nil {
// 						// No last build status
// 						fmt.Println("  -  Pending ", job.Name(), pendingBuild.Name(), pendingBuild.Status())
// 					}
// 				}
// 				// runningBuilds, err := job.GetRunningBuildsBySerialGroup([]string{job.Name()})
// 				// for _, runningBuild := range runningBuilds {
// 				// 	if runningBuild != nil {
// 				// 		// No last build status
// 				// 		fmt.Println("  -  Running ", job.Name(), runningBuild.Name(), runningBuild.Status())
// 				// 	}
// 				// }
//
// 			}
// 		}
//
// 	}
// 	return
// }
//
// // OrphanedContainers
// func ShowContainers(dbConn atcDb.Conn, lockFactory lock.LockFactory) {
// 	dbContainerRepository := atcDb.NewContainerRepository(dbConn)
// 	dbBuildFactory := atcDb.NewBuildFactory(dbConn, lockFactory)
// 	failedContainers, _ := dbContainerRepository.FindFailedContainers()
// 	creatingContainer, createdContainer, destroyingContainer, _ := dbContainerRepository.FindOrphanedContainers()
//
// 	fmt.Println("### Failed")
// 	for _, container := range failedContainers {
// 		fmt.Println(container.ID(), container.WorkerName(), container.Handle(), container.Metadata().JobName, container.Metadata().StepName)
// 	}
//
// 	fmt.Println("### Creating")
// 	for _, container := range creatingContainer {
// 		fmt.Println(container.ID(), container.WorkerName(), container.Handle(), container.Metadata().JobName, container.Metadata().StepName)
// 	}
//
// 	fmt.Println("### Created")
// 	for _, container := range createdContainer {
// 		build, _, _ := dbBuildFactory.Build(container.Metadata().BuildID)
// 		if build != nil {
// 			fmt.Println(container.ID(), container.WorkerName(), container.Handle(), container.Metadata().JobName, container.Metadata().StepName, container.Metadata().PipelineName, container.Metadata().JobName, container.Metadata().BuildName, build.Status())
// 		} else {
// 			fmt.Println(container.ID(), container.WorkerName(), container.Handle(), container.Metadata().JobName, container.Metadata().StepName, container.Metadata().PipelineName, container.Metadata().JobName)
// 		}
// 	}
//
// 	fmt.Println("### Destroying")
// 	for _, container := range destroyingContainer {
// 		fmt.Println(container.ID(), container.WorkerName(), container.Handle(), container.Metadata().JobName, container.Metadata().StepName)
// 	}
// }
//
// // Can display pipeline and end build status
// func ShowPipelines(dbConn atcDb.Conn, lockFactory lock.LockFactory) {
// 	dbPipelineFactory := atcDb.NewPipelineFactory(dbConn, lockFactory)
// 	pipelines, err := dbPipelineFactory.AllPipelines()
// 	if err != nil {
// 		fmt.Println(err)
// 	}
// 	for _, pipeline := range pipelines {
// 		fmt.Println(pipeline.TeamName(), pipeline.Name())
// 		jobs, err := pipeline.Jobs()
// 		if err != nil {
// 			fmt.Println(err)
// 			continue
// 		}
//
// 		for _, job := range jobs {
// 			// build, nextBuild, err := job.FinishedAndNextBuild()
// 			build, _, err := job.FinishedAndNextBuild()
// 			if err != nil {
// 				fmt.Println(err)
// 				continue
// 			}
//
// 			if build == nil {
// 				// No last build status
// 				continue
// 			} else {
// 				if build.Status() != "succeeded" {
// 					fmt.Println("  - ", job.Name(), build.Name(), build.Status())
// 				}
// 			}
// 			// input.Metadata : all data passed to the input, like commit info for git
// 		}
//
// 	}
// 	return
// }
//
// // able to display running builds and his inputs
// func ShowBuilds(dbConn atcDb.Conn, lockFactory lock.LockFactory) {
//
// 	dbBuildFactory := atcDb.NewBuildFactory(dbConn, lockFactory)
// 	teamFactory := atcDb.NewTeamFactory(dbConn, lockFactory)
// 	builds, _ := dbBuildFactory.GetAllStartedBuilds()
// 	for _, build := range builds {
//
// 		//debug filter
// 		if build.TeamName() != "pages-jaunes" {
// 			continue
// 		}
//
// 		fmt.Println(build.ID(), build.TeamName(), build.PipelineName(), build.JobName(), build.Name(), build.Status())
//
// 		inputs, outputs, _ := build.Resources()
//
// 		fmt.Println("Inputs :")
// 		for _, input := range inputs {
// 			fmt.Println("  - ", input.Resource, input.Type, input.Version)
// 			// input.Metadata : all data passed to the input, like commit info for git
// 		}
// 		fmt.Println("Outputs :")
// 		for _, output := range outputs {
// 			fmt.Println("  - ", output.Resource, output.Type, output.Version)
// 		}
//
// 		fmt.Println("Containers :")
// 		team := teamFactory.GetByID(build.TeamID())
// 		containers, _ := team.FindContainersByMetadata(atcDb.ContainerMetadata{PipelineID: build.PipelineID(), JobID: build.JobID(), BuildID: build.ID()})
// 		for _, container := range containers {
// 			fmt.Println(" - ", container.ID(), container.WorkerName(), container.Metadata())
// 		}
//
// 	}
// 	return
// }
