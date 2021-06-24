/*
Copyright 2019 The Vitess Authors.

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

package vreplstress

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"

	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/test/endtoend/onlineddl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testcase struct {
	name             string
	prepareStatement string
	alterStatement   string
	expectFailure    bool
}

var (
	clusterInstance      *cluster.LocalProcessCluster
	vtParams             mysql.ConnParams
	evaluatedMysqlParams *mysql.ConnParams

	directDDLStrategy     = "direct"
	onlineDDLStrategy     = "online -vreplication-test-suite -skip-topo"
	hostname              = "localhost"
	keyspaceName          = "ks"
	cell                  = "zone1"
	shards                []cluster.Shard
	schemaChangeDirectory = ""
	tableName             = "stress_test"
	afterTableName        = "stress_test_after"
	cleanupStatements     = []string{
		`DROP TABLE IF EXISTS stress_test`,
		`DROP TABLE IF EXISTS stress_test_before`,
		`DROP TABLE IF EXISTS stress_test_after`,
	}
	createStatement = `
		CREATE TABLE stress_test (
			id bigint(20) not null,
			id_negative bigint(20) not null,
			rand_text varchar(40) not null default '',
			rand_num bigint unsigned not null,
			nullable_num int default null,
			hint_col varchar(64) not null default '',
			created_timestamp timestamp not null default current_timestamp,
			updates int unsigned not null default 0,
			PRIMARY KEY (id)
		) ENGINE=InnoDB
	`
	testCases = []testcase{
		{
			name:             "trivial PK",
			prepareStatement: "",
			alterStatement:   "engine=innodb",
		},
		{
			name:             "negative UK, no PK",
			prepareStatement: "add unique key negative_uidx(id_negative)",
			alterStatement:   "drop primary key",
		},
		{
			name:             "negative UK, different PK",
			prepareStatement: "add unique key negative_uidx(id_negative)",
			alterStatement:   "drop primary key, add primary key(rand_text(40))",
		},
		{
			name:             "text UK, no PK",
			prepareStatement: "add unique key text_uidx(rand_text(40))",
			alterStatement:   "drop primary key",
		},
		{
			name:             "multicolumn UK 1, no PK",
			prepareStatement: "add unique key text_uidx(rand_text(40), id_negative)",
			alterStatement:   "drop primary key",
		},
		{
			name:             "multicolumn UK 2, no PK",
			prepareStatement: "add unique key text_uidx(id_negative, rand_text(40))",
			alterStatement:   "drop primary key",
		},
		{
			name:             "multicolumn UK 3, no PK",
			prepareStatement: "add unique key text_uidx(rand_num, rand_text(40))",
			alterStatement:   "drop primary key",
		},
		{
			name:             "multiple UK choices",
			prepareStatement: "add unique key text_uidx(rand_num, rand_text(40)), add unique key negative_uidx(id_negative)",
			alterStatement:   "drop primary key, add primary key(updates, id)",
		},
		{
			name:             "multiple UK choices including nullable",
			prepareStatement: "add unique key text_uidx(rand_num, rand_text(40)), add unique key nullable_uidx(nullable_num, id_negative), add unique key negative_uidx(id_negative)",
			alterStatement:   "drop primary key, add primary key(updates, id)",
		},
		{
			name:             "fail; no shared UK",
			prepareStatement: "add unique key negative_uidx(id_negative)",
			alterStatement:   "drop primary key, drop key negative_uidx, add primary key(rand_text(40)), add unique key negtext_uidx(id_negative, rand_text(40))",
			expectFailure:    true,
		},
		{
			name:             "fail; only nullable shared uk",
			prepareStatement: "add unique key nullable_uidx(nullable_num)",
			alterStatement:   "drop primary key",
			expectFailure:    true,
		},
	}
	alterHintStatement = `
		alter table stress_test modify hint_col varchar(64) not null default '%s'
	`

	insertRowStatement = `
		INSERT IGNORE INTO stress_test (id, id_negative, rand_text, rand_num) VALUES (%d, %d, concat(left(md5(rand()), 8), '_', %d), floor(rand()*1000000))
	`
	updateRowStatement = `
		UPDATE stress_test SET updates=updates+1 WHERE id=%d
	`
	deleteRowStatement = `
		DELETE FROM stress_test WHERE id=%d
	`
	selectCountFromTable = `
		SELECT count(*) as c FROM stress_test
	`
	selectCountFromTableAfter = `
		SELECT count(*) as c FROM stress_test_after
	`
	selectBeforeTable = `
		SELECT * FROM stress_test_before order by id
	`
	selectAfterTable = `
		SELECT * FROM stress_test_after order by id
	`
	truncateStatement = `
		TRUNCATE TABLE stress_test
	`
)

const (
	maxTableRows   = 4096
	maxConcurrency = 5
)

func getTablet() *cluster.Vttablet {
	return clusterInstance.Keyspaces[0].Shards[0].Vttablets[0]
}

func mysqlParams() *mysql.ConnParams {
	if evaluatedMysqlParams != nil {
		return evaluatedMysqlParams
	}
	evaluatedMysqlParams = &mysql.ConnParams{
		Uname:      "vt_dba",
		UnixSocket: path.Join(os.Getenv("VTDATAROOT"), fmt.Sprintf("/vt_%010d", getTablet().TabletUID), "/mysql.sock"),
		DbName:     fmt.Sprintf("vt_%s", keyspaceName),
	}
	return evaluatedMysqlParams
}

func TestMain(m *testing.M) {
	defer cluster.PanicHandler(nil)
	flag.Parse()

	exitcode, err := func() (int, error) {
		clusterInstance = cluster.NewCluster(cell, hostname)
		schemaChangeDirectory = path.Join("/tmp", fmt.Sprintf("schema_change_dir_%d", clusterInstance.GetAndReserveTabletUID()))
		defer os.RemoveAll(schemaChangeDirectory)
		defer clusterInstance.Teardown()

		if _, err := os.Stat(schemaChangeDirectory); os.IsNotExist(err) {
			_ = os.Mkdir(schemaChangeDirectory, 0700)
		}

		clusterInstance.VtctldExtraArgs = []string{
			"-schema_change_dir", schemaChangeDirectory,
			"-schema_change_controller", "local",
			"-schema_change_check_interval", "1",
			"-online_ddl_check_interval", "3s",
		}

		clusterInstance.VtTabletExtraArgs = []string{
			"-enable-lag-throttler",
			"-throttle_threshold", "1s",
			"-heartbeat_enable",
			"-heartbeat_interval", "250ms",
			"-migration_check_interval", "5s",
		}
		clusterInstance.VtGateExtraArgs = []string{
			"-ddl_strategy", "online",
		}

		if err := clusterInstance.StartTopo(); err != nil {
			return 1, err
		}

		// Start keyspace
		keyspace := &cluster.Keyspace{
			Name: keyspaceName,
		}

		// No need for replicas in this stress test
		if err := clusterInstance.StartKeyspace(*keyspace, []string{"1"}, 0, false); err != nil {
			return 1, err
		}

		vtgateInstance := clusterInstance.NewVtgateInstance()
		// set the gateway we want to use
		vtgateInstance.GatewayImplementation = "tabletgateway"
		// Start vtgate
		if err := vtgateInstance.Setup(); err != nil {
			return 1, err
		}
		// ensure it is torn down during cluster TearDown
		clusterInstance.VtgateProcess = *vtgateInstance
		vtParams = mysql.ConnParams{
			Host: clusterInstance.Hostname,
			Port: clusterInstance.VtgateMySQLPort,
		}

		return m.Run(), nil
	}()
	if err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	} else {
		os.Exit(exitcode)
	}

}

func TestSchemaChange(t *testing.T) {
	defer cluster.PanicHandler(t)

	shards = clusterInstance.Keyspaces[0].Shards
	require.Equal(t, 1, len(shards))

	for _, testcase := range testCases {
		require.NotEmpty(t, testcase.name)
		t.Run(testcase.name, func(t *testing.T) {
			t.Run("create schema", func(t *testing.T) {
				assert.Equal(t, 1, len(clusterInstance.Keyspaces[0].Shards))
				testWithInitialSchema(t)
			})
			t.Run("prepare table", func(t *testing.T) {
				if testcase.prepareStatement != "" {
					fullStatement := fmt.Sprintf("alter table %s %s", tableName, testcase.prepareStatement)
					onlineddl.VtgateExecDDL(t, &vtParams, directDDLStrategy, fullStatement, "")
				}
			})
			t.Run("init table data", func(t *testing.T) {
				initTable(t)
			})
			t.Run("migrate", func(t *testing.T) {
				require.NotEmpty(t, testcase.alterStatement)

				hintText := fmt.Sprintf("hint-after-alter-%d", rand.Int31n(int32(maxTableRows)))
				hintStatement := fmt.Sprintf(alterHintStatement, hintText)
				fullStatement := fmt.Sprintf("%s, %s", hintStatement, testcase.alterStatement)

				ctx, cancel := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					runMultipleConnections(ctx, t)
				}()
				uuid := testOnlineDDLStatement(t, fullStatement, onlineDDLStrategy, "vtgate", hintText)
				expectStatus := schema.OnlineDDLStatusComplete
				if testcase.expectFailure {
					expectStatus = schema.OnlineDDLStatusFailed
				}
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, expectStatus)
				cancel() // will cause runMultipleConnections() to terminate
				wg.Wait()
				if !testcase.expectFailure {
					testSelectTableMetrics(t)
				}
			})
		})
	}
}

func testWithInitialSchema(t *testing.T) {
	// Create the stress table
	for _, statement := range cleanupStatements {
		err := clusterInstance.VtctlclientProcess.ApplySchema(keyspaceName, statement)
		require.Nil(t, err)
	}
	err := clusterInstance.VtctlclientProcess.ApplySchema(keyspaceName, createStatement)
	require.Nil(t, err)

	// Check if table is created
	checkTable(t, tableName)
}

// testOnlineDDLStatement runs an online DDL, ALTER statement
func testOnlineDDLStatement(t *testing.T, alterStatement string, ddlStrategy string, executeStrategy string, expectHint string) (uuid string) {
	if executeStrategy == "vtgate" {
		row := onlineddl.VtgateExecDDL(t, &vtParams, ddlStrategy, alterStatement, "").Named().Row()
		if row != nil {
			uuid = row.AsString("uuid", "")
		}
	} else {
		var err error
		uuid, err = clusterInstance.VtctlclientProcess.ApplySchemaWithOutput(keyspaceName, alterStatement, cluster.VtctlClientParams{DDLStrategy: ddlStrategy})
		assert.NoError(t, err)
	}
	uuid = strings.TrimSpace(uuid)
	fmt.Println("# Generated UUID (for debug purposes):")
	fmt.Printf("<%s>\n", uuid)

	strategySetting, err := schema.ParseDDLStrategy(ddlStrategy)
	assert.NoError(t, err)

	status := schema.OnlineDDLStatusComplete
	if !strategySetting.Strategy.IsDirect() {
		status = onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, 20*time.Second, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
	}

	if expectHint != "" && status == schema.OnlineDDLStatusComplete {
		checkMigratedTable(t, afterTableName, expectHint)
	}
	return uuid
}

// checkTable checks the number of tables in the first two shards.
func checkTable(t *testing.T, showTableName string) {
	for i := range clusterInstance.Keyspaces[0].Shards {
		checkTablesCount(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], showTableName, 1)
	}
}

// checkTablesCount checks the number of tables in the given tablet
func checkTablesCount(t *testing.T, tablet *cluster.Vttablet, showTableName string, expectCount int) {
	query := fmt.Sprintf(`show tables like '%%%s%%';`, showTableName)
	queryResult, err := tablet.VttabletProcess.QueryTablet(query, keyspaceName, true)
	require.Nil(t, err)
	assert.Equal(t, expectCount, len(queryResult.Rows))
}

// checkMigratedTables checks the CREATE STATEMENT of a table after migration
func checkMigratedTable(t *testing.T, tableName, expectHint string) {
	for i := range clusterInstance.Keyspaces[0].Shards {
		createStatement := getCreateTableStatement(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], tableName)
		assert.Contains(t, createStatement, expectHint)
	}
}

// getCreateTableStatement returns the CREATE TABLE statement for a given table
func getCreateTableStatement(t *testing.T, tablet *cluster.Vttablet, tableName string) (statement string) {
	queryResult, err := tablet.VttabletProcess.QueryTablet(fmt.Sprintf("show create table %s;", tableName), keyspaceName, true)
	require.Nil(t, err)

	assert.Equal(t, len(queryResult.Rows), 1)
	assert.Equal(t, len(queryResult.Rows[0]), 2) // table name, create statement
	statement = queryResult.Rows[0][1].ToString()
	return statement
}

func generateInsert(t *testing.T, conn *mysql.Conn) error {
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(insertRowStatement, id, -id, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)
	if err == nil && qr != nil {
		assert.Less(t, qr.RowsAffected, uint64(2))
	}
	return err
}

func generateUpdate(t *testing.T, conn *mysql.Conn) error {
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(updateRowStatement, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)
	if err == nil && qr != nil {
		assert.Less(t, qr.RowsAffected, uint64(2))
	}
	return err
}

func generateDelete(t *testing.T, conn *mysql.Conn) error {
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(deleteRowStatement, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)
	if err == nil && qr != nil {
		assert.Less(t, qr.RowsAffected, uint64(2))
	}
	return err
}

func runSingleConnection(ctx context.Context, t *testing.T, done *int64) {
	log.Infof("Running single connection")
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	_, err = conn.ExecuteFetch("set autocommit=1", 1000, true)
	require.Nil(t, err)
	_, err = conn.ExecuteFetch("set transaction isolation level read committed", 1000, true)
	require.Nil(t, err)

	for {
		if atomic.LoadInt64(done) == 1 {
			log.Infof("Terminating single connection")
			return
		}
		switch rand.Int31n(3) {
		case 0:
			err = generateInsert(t, conn)
		case 1:
			err = generateUpdate(t, conn)
		case 2:
			err = generateDelete(t, conn)
		}
		if err != nil {
			if strings.Contains(err.Error(), "disallowed due to rule: enforce blacklisted tables") {
				err = nil
			} else if strings.Contains(err.Error(), "doesn't exist") {
				// Table renamed to _before, due to -vreplication-test-suite flag
				err = nil
			}
		}
		assert.Nil(t, err)
		time.Sleep(10 * time.Millisecond)
	}
}

func runMultipleConnections(ctx context.Context, t *testing.T) {
	log.Infof("Running multiple connections")
	var done int64
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSingleConnection(ctx, t, &done)
		}()
	}
	<-ctx.Done()
	atomic.StoreInt64(&done, 1)
	log.Infof("Running multiple connections: done")
	wg.Wait()
	log.Infof("All connections cancelled")
}

func initTable(t *testing.T) {
	log.Infof("initTable begin")
	defer log.Infof("initTable complete")

	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	_, err = conn.ExecuteFetch(truncateStatement, 1000, true)
	require.Nil(t, err)

	for i := 0; i < maxTableRows/2; i++ {
		generateInsert(t, conn)
	}
	for i := 0; i < maxTableRows/4; i++ {
		generateUpdate(t, conn)
	}
	for i := 0; i < maxTableRows/4; i++ {
		generateDelete(t, conn)
	}
	{
		// Validate table is populated
		rs, err := conn.ExecuteFetch(selectCountFromTable, 1000, true)
		require.Nil(t, err)
		row := rs.Named().Row()
		require.NotNil(t, row)

		count := row.AsInt64("c", 0)
		require.NotZero(t, count)
		require.Less(t, count, int64(maxTableRows))

		fmt.Printf("# count rows in table: %d\n", count)
	}
}

func testSelectTableMetrics(t *testing.T) {
	{
		// Validate after table is populated
		rs := onlineddl.VtgateExecQuery(t, &vtParams, selectCountFromTableAfter, "")
		row := rs.Named().Row()
		require.NotNil(t, row)

		count := row.AsInt64("c", 0)
		require.NotZero(t, count)
		require.Less(t, count, int64(maxTableRows))

		fmt.Printf("# count rows in table (after): %d\n", count)
	}

	{
		selectBeforeFile := onlineddl.CreateTempScript(t, selectBeforeTable)
		defer os.Remove(selectBeforeFile)
		beforeOutput := onlineddl.MysqlClientExecFile(t, mysqlParams(), "", "", selectBeforeFile)

		selectAfterFile := onlineddl.CreateTempScript(t, selectAfterTable)
		defer os.Remove(selectAfterFile)
		afterOutput := onlineddl.MysqlClientExecFile(t, mysqlParams(), "", "", selectAfterFile)

		require.Equal(t, beforeOutput, afterOutput, "results mismatch: (%s) and (%s)", selectBeforeTable, selectAfterTable)
	}

	// rsBefore := onlineddl.VtgateExecQuery(t, &vtParams, selectBeforeTable, "")
	// rsAfter := onlineddl.VtgateExecQuery(t, &vtParams, selectAfterTable, "")
	// assert.Equal(t, rsBefore.Rows, rsAfter.Rows)
}
