/*
Copyright 2016, Cossack Labs Limited

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package main is entry point for AcraServer utility. AcraServer is the server responsible for decrypting all
// the database responses and forwarding them back to clients. AcraServer waits to connection from AcraConnector.
// When the first AcraConnector connection arrives, AcraServer initialises secure communication via TLS or
// Themis Secure Session. After a successful initialisation of the session, AcraServer creates a database connection
// and starts forwarding all the requests coming from AcraConnector into the database.
// Every incoming request to AcraServer is passed through AcraCensor (Acra's firewall). AcraCensor will pass allowed
// queries and return error on forbidden ones.
// Upon receiving the answer, AcraServer attempts to unpack the AcraStruct and to decrypt the payload. After that,
// AcraServer will replace the AcraStruct with the decrypted payload, change the packet's length, and return
// the answer to the application via AcraConnector.
// If AcraServer detects a poison record within the AcraStruct's decryption stream, AcraServer will either
// shut down the decryption, run an alarm script, or do both, depending on the pre-set parameters.
//
// https://github.com/cossacklabs/acra/wiki/How-AcraServer-works
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"syscall"
	"time"

	"github.com/cossacklabs/acra/cmd"
	"github.com/cossacklabs/acra/keystore"
	"github.com/cossacklabs/acra/keystore/filesystem"
	"github.com/cossacklabs/acra/logging"
	"github.com/cossacklabs/acra/network"
	"github.com/cossacklabs/acra/utils"
	log "github.com/sirupsen/logrus"
)

var restartSignalsChannel chan os.Signal
var errorSignalChannel chan os.Signal
var authPath *string

// For testing purposes only, allows to skip checking TLS certificate when connecting to database.
const (
	TEST_MODE = "true"
)

// TestOnly is set in compile time for running integration tests
var TestOnly = "false"

// Constants used by AcraServer.
const (
	DEFAULT_ACRASERVER_WAIT_TIMEOUT = 10
	GRACEFUL_ENV                    = "GRACEFUL_RESTART"
	DESCRIPTOR_ACRA                 = 3
	DESCRIPTOR_API                  = 4
	SERVICE_NAME                    = "acra-server"
)

// DEFAULT_CONFIG_PATH relative path to config which will be parsed as default
var DEFAULT_CONFIG_PATH = utils.GetConfigPathByName(SERVICE_NAME)

// ErrWaitTimeout error indicates that server was shutdown and waited N seconds while shutting down all connections.
var ErrWaitTimeout = errors.New("timeout")

func main() {
	config := NewConfig()
	loggingFormat := flag.String("logging_format", "plaintext", "Logging format: plaintext, json or CEF")
	logging.CustomizeLogging(*loggingFormat, SERVICE_NAME)
	log.Infof("Starting service %v", SERVICE_NAME)

	dbHost := flag.String("db_host", "", "Host to db")
	dbPort := flag.Int("db_port", 5432, "Port to db")

	prometheusAddress := flag.String("prometheus_metrics_address", "", "URL of Prometheus server for AcraConnector to upload stats and metrics (upload address is <URL>/metrics)")

	host := flag.String("incoming_connection_host", cmd.DEFAULT_ACRA_HOST, "Host for AcraServer")
	port := flag.Int("incoming_connection_port", cmd.DEFAULT_ACRASERVER_PORT, "Port for AcraServer")
	apiPort := flag.Int("incoming_connection_api_port", cmd.DEFAULT_ACRASERVER_API_PORT, "Port for AcraServer for HTTP API")

	keysDir := flag.String("keys_dir", keystore.DefaultKeyDirShort, "Folder from which will be loaded keys")
	keysCacheSize := flag.Int("keystore_cache_size", keystore.INFINITE_CACHE_SIZE, "Count of keys that will be stored in in-memory LRU cache in encrypted form. 0 - no limits, -1 - turn off cache")

	pgHexFormat := flag.Bool("pgsql_hex_bytea", false, "Hex format for Postgresql bytea data (default)")
	pgEscapeFormat := flag.Bool("pgsql_escape_bytea", false, "Escape format for Postgresql bytea data")

	secureSessionID := flag.String("securesession_id", "acra_server", "Id that will be sent in secure session")

	flag.Bool("acrastruct_wholecell_enable", true, "Acrastruct will stored in whole data cell")
	injectedcell := flag.Bool("acrastruct_injectedcell_enable", false, "Acrastruct may be injected into any place of data cell")

	debugServer := flag.Bool("ds", false, "Turn on http debug server")
	closeConnectionTimeout := flag.Int("incoming_connection_close_timeout", DEFAULT_ACRASERVER_WAIT_TIMEOUT, "Time that AcraServer will wait (in seconds) on restart before closing all connections")

	detectPoisonRecords := flag.Bool("poison_detect_enable", true, "Turn on poison record detection, if server shutdown is disabled, AcraServer logs the poison record detection and returns decrypted data")
	stopOnPoison := flag.Bool("poison_shutdown_enable", false, "On detecting poison record: log about poison record detection, stop and shutdown")
	scriptOnPoison := flag.String("poison_run_script_file", "", "On detecting poison record: log about poison record detection, execute script, return decrypted data")

	withZone := flag.Bool("zonemode_enable", false, "Turn on zone mode")
	enableHTTPAPI := flag.Bool("http_api_enable", false, "Enable HTTP API")

	useTLS := flag.Bool("acraconnector_tls_transport_enable", false, "Use tls to encrypt transport between AcraServer and AcraConnector/client")
	tlsKey := flag.String("tls_key", "", "Path to private key that will be used in TLS handshake with AcraConnector as server's key and Postgresql as client's key")
	tlsCert := flag.String("tls_cert", "", "Path to tls certificate")
	tlsCA := flag.String("tls_ca", "", "Path to root certificate which will be used with system root certificates to validate Postgresql's and AcraConnector's certificate")
	tlsDbSNI := flag.String("tls_db_sni", "", "Expected Server Name (SNI) from Postgresql")
	tlsAuthType := flag.Int("tls_auth", int(tls.RequireAndVerifyClientCert), "Set authentication mode that will be used in TLS connection with Postgresql. Values in range 0-4 that set auth type (https://golang.org/pkg/crypto/tls/#ClientAuthType). Default is tls.RequireAndVerifyClientCert")
	noEncryptionTransport := flag.Bool("acraconnector_transport_encryption_disable", false, "Use raw transport (tcp/unix socket) between AcraServer and AcraConnector/client (don't use this flag if you not connect to database with ssl/tls")
	clientID := flag.String("client_id", "", "Expected client ID of AcraConnector in mode without encryption")
	acraConnectionString := flag.String("incoming_connection_string", network.BuildConnectionString(cmd.DEFAULT_ACRA_CONNECTION_PROTOCOL, cmd.DEFAULT_ACRA_HOST, cmd.DEFAULT_ACRASERVER_PORT, ""), "Connection string like tcp://x.x.x.x:yyyy or unix:///path/to/socket")
	acraAPIConnectionString := flag.String("incoming_connection_api_string", network.BuildConnectionString(cmd.DEFAULT_ACRA_CONNECTION_PROTOCOL, cmd.DEFAULT_ACRA_HOST, cmd.DEFAULT_ACRASERVER_API_PORT, ""), "Connection string for api like tcp://x.x.x.x:yyyy or unix:///path/to/socket")
	authPath = flag.String("auth_keys", cmd.DEFAULT_ACRA_AUTH_PATH, "Path to basic auth passwords. To add user, use: `./acra-authmanager --set --user <user> --pwd <pwd>`")

	useMysql := flag.Bool("mysql_enable", false, "Handle MySQL connections")
	usePostgresql := flag.Bool("postgresql_enable", false, "Handle Postgresql connections (default true)")
	censorConfig := flag.String("acracensor_config_file", "", "Path to AcraCensor configuration file")

	verbose := flag.Bool("v", false, "Log to stderr all INFO, WARNING and ERROR logs")
	debug := flag.Bool("d", false, "Log everything to stderr")

	err := cmd.Parse(DEFAULT_CONFIG_PATH, SERVICE_NAME)
	if err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantReadServiceConfig).
			Errorln("Can't parse args")
		os.Exit(1)
	}

	// if log format was overridden
	logging.CustomizeLogging(*loggingFormat, SERVICE_NAME)

	log.Infof("Validating service configuration...")
	cmd.ValidateClientID(*secureSessionID)

	if *host != cmd.DEFAULT_ACRA_HOST || *port != cmd.DEFAULT_ACRASERVER_PORT {
		*acraConnectionString = network.BuildConnectionString("tcp", *host, *port, "")
	}
	if *apiPort != cmd.DEFAULT_ACRASERVER_API_PORT {
		*acraConnectionString = network.BuildConnectionString("tcp", *host, *apiPort, "")
	}

	if *dbHost == "" {
		log.WithField(logging.FieldKeyEventCode, logging.EventCodeErrorWrongConfiguration).
			Errorln("db_host is empty: you must specify db_host")
		flag.Usage()
		return
	}

	if err := config.SetMySQL(*useMysql); err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorWrongConfiguration).
			Errorln("Can't set MySQL support")
		os.Exit(1)
	}
	if err := config.SetPostgresql(*usePostgresql); err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorWrongConfiguration).
			Errorln("Can't set PostgreSQL support")
		os.Exit(1)
	}

	if err := config.SetCensor(*censorConfig); err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCensorSetupError).
			Errorln("Can't setup censor")
		os.Exit(1)
	}

	// now it's stub as default values
	config.SetDetectPoisonRecords(*detectPoisonRecords)
	config.SetStopOnPoison(*stopOnPoison)
	config.SetScriptOnPoison(*scriptOnPoison)
	config.SetWithZone(*withZone)
	config.SetDBHost(*dbHost)
	config.SetDBPort(*dbPort)
	config.SetConnectorHost(*host)
	config.SetConnectorPort(*port)
	config.SetConnectorAPIPort(*apiPort)
	config.SetKeysDir(*keysDir)
	config.SetServerID([]byte(*secureSessionID))
	config.SetAcraConnectionString(*acraConnectionString)
	config.SetAcraAPIConnectionString(*acraAPIConnectionString)
	config.SetTLSServerCertPath(*tlsCert)
	config.SetTLSServerKeyPath(*tlsKey)
	config.SetWholeMatch(!(*injectedcell))
	config.SetEnableHTTPAPI(*enableHTTPAPI)
	config.SetConfigPath(DEFAULT_CONFIG_PATH)
	config.SetDebug(*debug)

	if *pgHexFormat || !*pgEscapeFormat {
		config.SetByteaFormat(HEX_BYTEA_FORMAT)
	} else {
		config.SetByteaFormat(ESCAPE_BYTEA_FORMAT)
	}

	log.Infof("Initialising keystore...")
	masterKey, err := keystore.GetMasterKeyFromEnvironment()
	if err != nil {
		log.WithError(err).Errorln("can't load master key")
		os.Exit(1)
	}
	scellEncryptor, err := keystore.NewSCellKeyEncryptor(masterKey)
	if err != nil {
		log.WithError(err).Errorln("can't init scell encryptor")
		os.Exit(1)
	}
	keyStore, err := filesystem.NewFileSystemKeyStoreWithCacheSize(*keysDir, scellEncryptor, *keysCacheSize)
	if err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantInitKeyStore).
			Errorln("Can't initialise keystore")
		os.Exit(1)
	}
	log.Infof("Keystore init OK")

	log.Infof("Configuring transport...")
	var tlsConfig *tls.Config
	if *useTLS || *tlsKey != "" {
		tlsConfig, err = network.NewTLSConfig(network.SNIOrHostname(*tlsDbSNI, *dbHost), *tlsCA, *tlsKey, *tlsCert, tls.ClientAuthType(*tlsAuthType))
		if err != nil {
			log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorTransportConfiguration).
				Errorln("Configuration error: can't get config for TLS")
			os.Exit(1)
		}
		// need for testing with mysql docker container that always generate new certificates
		if TestOnly == TEST_MODE {
			tlsConfig.InsecureSkipVerify = true
			tlsConfig.ClientAuth = tls.NoClientCert
			log.Warningln("Skip verifying TLS certificate, use for tests only!")
		}
	}
	config.SetTLSConfig(tlsConfig)
	if *useTLS {
		log.Println("Selecting transport: use TLS transport wrapper")
		config.ConnectionWrapper, err = network.NewTLSConnectionWrapper([]byte(*clientID), tlsConfig)
		if err != nil {
			log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorTransportConfiguration).
				Errorln("Configuration error: can't initialise TLS connection wrapper")
			os.Exit(1)
		}
	} else if *noEncryptionTransport {
		if *clientID == "" && !*withZone {
			log.WithField(logging.FieldKeyEventCode, logging.EventCodeErrorTransportConfiguration).
				Errorln("Configuration error: without zone mode and without encryption you must set <client_id> which will be used to connect from AcraConnector to AcraServer")
			os.Exit(1)
		}
		log.Infof("Selecting transport: use raw transport wrapper")
		config.ConnectionWrapper = &network.RawConnectionWrapper{ClientID: []byte(*clientID)}
	} else {
		log.Infof("Selecting transport: use Secure Session transport wrapper")
		config.ConnectionWrapper, err = network.NewSecureSessionConnectionWrapper(keyStore)
		if err != nil {
			log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorTransportConfiguration).
				Errorln("Configuration error: can't initialize secure session connection wrapper")
			os.Exit(1)
		}
	}

	log.Debugf("Registering process signal handlers")
	sigHandlerSIGTERM, err := cmd.NewSignalHandler([]os.Signal{os.Interrupt, syscall.SIGTERM})
	errorSignalChannel = sigHandlerSIGTERM.GetChannel()
	if err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantRegisterSignalHandler).
			Errorln("System error: can't register SIGTERM handler")
		os.Exit(1)
	}

	sigHandlerSIGHUP, err := cmd.NewSignalHandler([]os.Signal{syscall.SIGHUP})
	restartSignalsChannel = sigHandlerSIGHUP.GetChannel()
	if err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantRegisterSignalHandler).
			Errorln("System error: can't register SIGHUP handler")
		os.Exit(1)
	}

	var server *SServer
	server, err = NewServer(config, keyStore, errorSignalChannel, restartSignalsChannel)
	if err != nil {
		log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantStartService).
			Errorln("System error: can't start %s", SERVICE_NAME)
		panic(err)
	}

	if os.Getenv(GRACEFUL_ENV) == "true" {
		server.fddACRA = DESCRIPTOR_ACRA
		server.fdAPI = DESCRIPTOR_API
		log.Debugf("Will be using GRACEFUL_RESTART if configured from WebUI")
	}

	if *debugServer {
		//start http server for pprof
		debugServerAddress := "127.0.0.1:6060"
		log.Debugf("Starting Debug server on %s", debugServerAddress)
		go func() {
			err := http.ListenAndServe(debugServerAddress, nil)
			if err != nil {
				log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantStartService).
					Errorln("System error: got error from Debug Server")
			}
		}()
	}

	if *prometheusAddress != "" {
		prometheusListener, err := cmd.RunPrometheusHTTPHandler(*prometheusAddress)
		if err != nil {
			panic(err)
		}
		log.Infof("Configured to send metrics and stats to `prometheus_metrics_address`")
		sigHandlerSIGHUP.AddListener(prometheusListener)
		sigHandlerSIGTERM.AddListener(prometheusListener)
	}

	go sigHandlerSIGTERM.Register()
	sigHandlerSIGTERM.AddCallback(func() {
		log.Infof("Received incoming SIGTERM or SIGINT signal")
		log.Debugf("Stop accepting new connections, waiting until current connections close")
		// Stop accepting new connections
		server.StopListeners()
		// Wait a maximum of N seconds for existing connections to finish
		err := server.WaitWithTimeout(time.Duration(*closeConnectionTimeout) * time.Second)
		if err == ErrWaitTimeout {
			log.Warningf("Server shutdown Timeout: %d active connections will be cut", server.ConnectionsCounter())
			server.Close()
			os.Exit(1)
		}
		server.Close()
		log.Infof("Server graceful shutdown completed, bye PID: %v", os.Getpid())
		os.Exit(0)
	})

	sigHandlerSIGHUP.AddCallback(func() {
		log.Infof("Received incoming SIGHUP signal")
		log.Debugf("Stop accepting new connections, waiting until current connections close")

		// Stop accepting requests
		server.StopListeners()

		// Get socket file descriptor to pass it to fork
		var fdACRA, fdAPI uintptr
		fdACRA, err = network.ListenerFileDescriptor(server.listenerACRA)
		if err != nil {
			log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantGetFileDescriptor).
				Fatalln("System error: failed to get acra-socket file descriptor:", err)
		}
		if *withZone || *enableHTTPAPI {
			fdAPI, err = network.ListenerFileDescriptor(server.listenerAPI)
			if err != nil {
				log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantGetFileDescriptor).
					Fatalln("System error: failed to get api-socket file descriptor:", err)
			}
		}

		// Set env flag for forked process
		os.Setenv(GRACEFUL_ENV, "true")
		execSpec := &syscall.ProcAttr{
			Env:   os.Environ(),
			Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd(), fdACRA, fdAPI},
		}

		log.Debugf("Forking new process of %s", SERVICE_NAME)

		// Fork new process
		var fork, err = syscall.ForkExec(os.Args[0], os.Args, execSpec)
		if err != nil {
			log.WithError(err).WithField(logging.FieldKeyEventCode, logging.EventCodeErrorCantForkProcess).
				Fatalln("System error: failed to fork new process", err)
		}
		log.Infof("%s process forked to PID: %v", SERVICE_NAME, fork)

		// Wait a maximum of N seconds for existing connections to finish
		err = server.WaitWithTimeout(time.Duration(*closeConnectionTimeout) * time.Second)
		if err == ErrWaitTimeout {
			log.Warningf("Server shutdown Timeout: %d active connections will be cut", server.ConnectionsCounter())
			os.Exit(0)
		}
		log.Infof("Server graceful restart completed, bye PID: %v", os.Getpid())

		// Stop the old server, all the connections have been closed and the new one is running
		os.Exit(0)
	})

	log.Infof("Start listening to connections. Current PID: %v", os.Getpid())

	if *debug {
		log.Infof("Enabling DEBUG log level")
		logging.SetLogLevel(logging.LOG_DEBUG)
	} else if *verbose {
		log.Infof("Enabling VERBOSE log level")
		logging.SetLogLevel(logging.LOG_VERBOSE)
	} else {
		log.Infof("Disabling future logs... Set -v -d to see logs")
		logging.SetLogLevel(logging.LOG_DISCARD)
	}

	if os.Getenv(GRACEFUL_ENV) == "true" {
		if *withZone || *enableHTTPAPI {
			go server.StartCommandsFromFileDescriptor(DESCRIPTOR_API)
		}
		go server.StartFromFileDescriptor(DESCRIPTOR_ACRA)
	} else {
		if *withZone || *enableHTTPAPI {
			go server.StartCommands()
		}
		go server.Start()
	}

	// on sighup we run callback that stop all listeners (that stop background goroutine of server.Start())
	// and try to restart acra-server and only after that exits
	sigHandlerSIGHUP.Register()
}
