package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/atc/atccmd"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/db/encryption"
	"github.com/concourse/atc/db/lock"
	"github.com/concourse/atc/metric"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

type MyATCCommand atccmd.ATCCommand

// copy private methodes from https://github.com/concourse/atc/blob/master/atccmd/command.go#L907
func (cmd *MyATCCommand) constructLockConn(driverName string) (*sql.DB, error) {
	dbConn, err := sql.Open(driverName, cmd.Postgres.ConnectionString())
	if err != nil {
		return nil, err
	}

	dbConn.SetMaxOpenConns(10)
	dbConn.SetMaxIdleConns(2)
	dbConn.SetConnMaxLifetime(0)

	return dbConn, nil
}

func (cmd *MyATCCommand) constructDBConn(
	driverName string,
	logger lager.Logger,
	newKey *encryption.Key,
	oldKey *encryption.Key,
	maxConn int,
	connectionName string,
	lockFactory lock.LockFactory,
) (db.Conn, error) {
	dbConn, err := db.Open(logger.Session("db"), driverName, cmd.Postgres.ConnectionString(), newKey, oldKey, connectionName, lockFactory)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database: %s", err)
	}

	// Instrument with Metrics
	dbConn = metric.CountQueries(dbConn)
	metric.Databases = append(metric.Databases, dbConn)

	// Instrument with Logging
	if cmd.LogDBQueries {
		dbConn = db.Log(logger.Session("log-conn"), dbConn)
	}

	// Prepare
	dbConn.SetMaxOpenConns(maxConn)

	return dbConn, nil
}

func connectDb() (db.Conn, lock.LockFactory) {

	psqlConfig := atccmd.PostgresConfig{Host: viper.GetString("host"), Port: uint16(viper.GetInt("port")), User: viper.GetString("user"), Password: viper.GetString("password"), Database: viper.GetString("database"), SSLMode: "disable"}
	cmd := MyATCCommand{Postgres: psqlConfig}
	fmt.Printf("DEBUG : %s\n", cmd.Postgres.ConnectionString())
	var driverName = "postgres"
	var newKey *encryption.Key
	var oldKey *encryption.Key
	var maxConns = 32
	var connectionName = "concourse-toolkit"

	lockConn, err := cmd.constructLockConn(driverName)
	if err != nil {
		fmt.Println(err)
		return nil, nil
	}
	lockFactory := lock.NewLockFactory(lockConn)

	lagerFlag := atccmd.LagerFlag{LogLevel: "debug"}
	mylogger, _ := lagerFlag.Logger("concourse-toolkit")
	dbConn, err := cmd.constructDBConn(driverName, mylogger, newKey, oldKey, maxConns, connectionName, lockFactory)
	if err != nil {
		fmt.Println(err)
		return nil, nil
	}
	return dbConn, lockFactory
}

func metricOrphanedContainers(promMetrics *PrometheusMetrics, dbConn db.Conn, lockFactory lock.LockFactory) {
	dbContainerRepository := db.NewContainerRepository(dbConn)
	dbBuildFactory := db.NewBuildFactory(dbConn, lockFactory)
	failedContainers, _ := dbContainerRepository.FindFailedContainers()
	creatingContainer, createdContainer, destroyingContainer, _ := dbContainerRepository.FindOrphanedContainers()
	var defaultTeam = ""

	for _, container := range failedContainers {
		team := defaultTeam
		if container.Metadata().BuildID != 0 {
			build, _, _ := dbBuildFactory.Build(container.Metadata().BuildID)
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
			build, _, _ := dbBuildFactory.Build(container.Metadata().BuildID)
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
			build, _, _ := dbBuildFactory.Build(container.Metadata().BuildID)
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
			build, _, _ := dbBuildFactory.Build(container.Metadata().BuildID)
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

func metricRunningTasks(promMetrics *PrometheusMetrics, dbConn db.Conn, lockFactory lock.LockFactory) {

	dbBuildFactory := db.NewBuildFactory(dbConn, lockFactory)
	teamFactory := db.NewTeamFactory(dbConn, lockFactory)

	builds, _ := dbBuildFactory.GetAllStartedBuilds()
	for _, build := range builds {
		team := teamFactory.GetByID(build.TeamID())
		containers, _ := team.FindContainersByMetadata(db.ContainerMetadata{PipelineID: build.PipelineID(), JobID: build.JobID(), BuildID: build.ID()})
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

func metricBuildsAndResources(promMetrics *PrometheusMetrics, dbConn db.Conn, lockFactory lock.LockFactory) {

	teamFactory := db.NewTeamFactory(dbConn, lockFactory)

	teams, _ := teamFactory.GetTeams()
	for _, team := range teams {

		pipelines, _ := team.Pipelines()
		for _, pipeline := range pipelines {

			resources, err := pipeline.Resources()
			if err != nil {
				fmt.Println(err)
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
						"team":       job.TeamName(),
						"pipeline":   job.PipelineName(),
						"job":        job.Name(),
						"status":     string(nextBuild.Status()),
						"start_time": nextBuild.StartTime().String(),
						"end_time":   nextBuild.EndTime().String(),
						"name":       nextBuild.Name(),
					}).Set(floatBuildName)
				}

				// Get finished builds
				if build != nil {
					// fmt.Println("  - ", job.Name(), build.Name(), build.Status())
					floatBuildName, _ := strconv.ParseFloat(build.Name(), 64)
					promMetrics.builds.With(prometheus.Labels{
						"team":       job.TeamName(),
						"pipeline":   job.PipelineName(),
						"job":        job.Name(),
						"status":     string(build.Status()),
						"start_time": build.StartTime().String(),
						"end_time":   build.EndTime().String(),
						"name":       build.Name(),
					}).Set(floatBuildName)

				}

				// Get pending builds
				pendingBuilds, _ := job.GetPendingBuilds()
				for _, pendingBuild := range pendingBuilds {
					if pendingBuild != nil {
						// No last build status
						// fmt.Println("  -  Pending ", job.Name(), pendingBuild.Name(), pendingBuild.Status())
						floatBuildName, _ := strconv.ParseFloat(pendingBuild.Name(), 64)
						promMetrics.builds.With(prometheus.Labels{
							"team":       job.TeamName(),
							"pipeline":   job.PipelineName(),
							"job":        job.Name(),
							"status":     string(pendingBuild.Status()),
							"start_time": pendingBuild.StartTime().String(),
							"end_time":   pendingBuild.EndTime().String(),
							"name":       pendingBuild.Name(),
						}).Set(floatBuildName)

					}
				}
			}
		}
	}
}

func metricWorkers(promMetrics *PrometheusMetrics, dbConn db.Conn) {

	dbWorkerFactory := db.NewWorkerFactory(dbConn)

	workers, _ := dbWorkerFactory.Workers()
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

func getSomeMetrics(promMetrics *PrometheusMetrics, dbConn db.Conn, lockFactory lock.LockFactory) {
	metricWorkers(promMetrics, dbConn)
	metricBuildsAndResources(promMetrics, dbConn, lockFactory)
	metricRunningTasks(promMetrics, dbConn, lockFactory)
	metricOrphanedContainers(promMetrics, dbConn, lockFactory)
}

func run(cmd *cobra.Command, args []string) {
	dbConn, lockFactory := connectDb()
	//Init DB connect
	// ShowPipelines(dbConn, lockFactory)
	// ShowTeams(dbConn, lockFactory)
	// fmt.Println("")
	// ShowContainers(dbConn, lockFactory)
	// fmt.Println("")
	// ShowBuilds(dbConn, lockFactory)

	promMetrics := NewMetrics()

	fmt.Printf("DEBUG : Exposing metrics on http://0.0.0.0:%s/metrics\n", viper.GetString("metrics-port"))
	config := &PrometheusConfig{BindIP: "0.0.0.0", BindPort: viper.GetString("metrics-port")}
	listener, err := net.Listen("tcp", config.bind())
	if err != nil {
		fmt.Println(err)
	}

	// go http.Serve(listener, promhttp.Handler())
	go http.Serve(listener, promhttp.Handler())

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

// func ShowTeams(dbConn db.Conn, lockFactory lock.LockFactory) {
// 	teamFactory := db.NewTeamFactory(dbConn, lockFactory)
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
// 					containers, _ := team.FindContainersByMetadata(db.ContainerMetadata{PipelineID: pipeline.ID(), JobID: job.ID(), BuildID: nextBuild.ID()})
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
// func ShowContainers(dbConn db.Conn, lockFactory lock.LockFactory) {
// 	dbContainerRepository := db.NewContainerRepository(dbConn)
// 	dbBuildFactory := db.NewBuildFactory(dbConn, lockFactory)
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
// func ShowPipelines(dbConn db.Conn, lockFactory lock.LockFactory) {
// 	dbPipelineFactory := db.NewPipelineFactory(dbConn, lockFactory)
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
// func ShowBuilds(dbConn db.Conn, lockFactory lock.LockFactory) {
//
// 	dbBuildFactory := db.NewBuildFactory(dbConn, lockFactory)
// 	teamFactory := db.NewTeamFactory(dbConn, lockFactory)
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
// 		containers, _ := team.FindContainersByMetadata(db.ContainerMetadata{PipelineID: build.PipelineID(), JobID: build.JobID(), BuildID: build.ID()})
// 		for _, container := range containers {
// 			fmt.Println(" - ", container.ID(), container.WorkerName(), container.Metadata())
// 		}
//
// 	}
// 	return
// }
