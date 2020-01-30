package canal

import (
	"flag"
	"fmt"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser"
	"github.com/siddontang/go-log/log"
	"github.com/steerben/go-mysql/mysql"
	"github.com/steerben/go-mysql/replication"
)

var testHost = flag.String("host", "127.0.0.1", "MySQL host")

func Test(t *testing.T) {
	TestingT(t)
}

type canalTestSuite struct {
	c *Canal
}

var _ = Suite(&canalTestSuite{})

func (s *canalTestSuite) SetUpSuite(c *C) {
	cfg := NewDefaultConfig()
	cfg.Addr = fmt.Sprintf("%s:3306", *testHost)
	cfg.User = "root"
	cfg.HeartbeatPeriod = 200 * time.Millisecond
	cfg.ReadTimeout = 300 * time.Millisecond
	cfg.Dump.ExecutionPath = "mysqldump"
	cfg.Dump.TableDB = "test"
	cfg.Dump.Tables = []string{"canal_test"}
	cfg.Dump.Where = "id>0"

	// include & exclude config
	cfg.IncludeTableRegex = make([]string, 1)
	cfg.IncludeTableRegex[0] = ".*\\.canal_test"
	cfg.ExcludeTableRegex = make([]string, 2)
	cfg.ExcludeTableRegex[0] = "mysql\\..*"
	cfg.ExcludeTableRegex[1] = ".*\\..*_inner"

	var err error
	s.c, err = NewCanal(cfg)
	c.Assert(err, IsNil)
	s.execute(c, "DROP TABLE IF EXISTS test.canal_test")
	sql := `
        CREATE TABLE IF NOT EXISTS test.canal_test (
			id int AUTO_INCREMENT,
			content blob DEFAULT NULL,
            name varchar(100),
			mi mediumint(8) NOT NULL DEFAULT 0,
			umi mediumint(8) unsigned NOT NULL DEFAULT 0,
            PRIMARY KEY(id)
            )ENGINE=innodb;
    `

	s.execute(c, sql)

	s.execute(c, "DELETE FROM test.canal_test")
	s.execute(c, "INSERT INTO test.canal_test (content, name, mi, umi) VALUES (?, ?, ?, ?), (?, ?, ?, ?), (?, ?, ?, ?)", "1", "a", 0, 0, `\0\ndsfasdf`, "b", 1, 16777215, "", "c", -1, 1)

	s.execute(c, "SET GLOBAL binlog_format = 'ROW'")

	s.c.SetEventHandler(&testEventHandler{c: c})
	go func() {
		set, _ := mysql.ParseGTIDSet("mysql", "")
		err = s.c.StartFromGTID(set)
		c.Assert(err, IsNil)
	}()
}

func (s *canalTestSuite) TearDownSuite(c *C) {
	// To test the heartbeat and read timeout,so need to sleep 1 seconds without data transmission
	c.Logf("Start testing the heartbeat and read timeout")
	time.Sleep(time.Second)

	if s.c != nil {
		s.c.Close()
		s.c = nil
	}
}

func (s *canalTestSuite) execute(c *C, query string, args ...interface{}) *mysql.Result {
	r, err := s.c.Execute(query, args...)
	c.Assert(err, IsNil)
	return r
}

type testEventHandler struct {
	DummyEventHandler
	c *C
}

func (h *testEventHandler) OnRow(e *RowsEvent) error {
	log.Infof("OnRow %s %v\n", e.Action, e.Rows)
	umi, ok := e.Rows[0][4].(uint32) // 4th col is umi. mysqldump gives uint64 instead of uint32
	if ok && (umi != 0 && umi != 1 && umi != 16777215) {
		return fmt.Errorf("invalid unsigned medium int %d", umi)
	}
	return nil
}

func (h *testEventHandler) String() string {
	return "testEventHandler"
}

func (h *testEventHandler) OnPosSynced(p mysql.Position, set mysql.GTIDSet, f bool) error {
	return nil
}

func (s *canalTestSuite) TestCanal(c *C) {
	<-s.c.WaitDumpDone()

	for i := 1; i < 10; i++ {
		s.execute(c, "INSERT INTO test.canal_test (name) VALUES (?)", fmt.Sprintf("%d", i))
	}
	s.execute(c, "INSERT INTO test.canal_test (mi,umi) VALUES (?,?), (?,?), (?,?)", 0, 0, -1, 16777215, 1, 1)
	s.execute(c, "ALTER TABLE test.canal_test ADD `age` INT(5) NOT NULL AFTER `name`")
	s.execute(c, "INSERT INTO test.canal_test (name,age) VALUES (?,?)", "d", "18")

	err := s.c.CatchMasterPos(10 * time.Second)
	c.Assert(err, IsNil)
}

func (s *canalTestSuite) TestCanalFilter(c *C) {
	// included
	sch, err := s.c.GetTable("test", "canal_test")
	c.Assert(err, IsNil)
	c.Assert(sch, NotNil)
	_, err = s.c.GetTable("not_exist_db", "canal_test")
	c.Assert(errors.Trace(err), Not(Equals), ErrExcludedTable)
	// excluded
	sch, err = s.c.GetTable("test", "canal_test_inner")
	c.Assert(errors.Cause(err), Equals, ErrExcludedTable)
	c.Assert(sch, IsNil)
	sch, err = s.c.GetTable("mysql", "canal_test")
	c.Assert(errors.Cause(err), Equals, ErrExcludedTable)
	c.Assert(sch, IsNil)
	sch, err = s.c.GetTable("not_exist_db", "not_canal_test")
	c.Assert(errors.Cause(err), Equals, ErrExcludedTable)
	c.Assert(sch, IsNil)
}

func TestCreateTableExp(t *testing.T) {
	cases := []string{
		"CREATE TABLE /*generated by server */ mydb.mytable (`id` int(10)) ENGINE=InnoDB",
		"CREATE TABLE `mydb`.`mytable` (`id` int(10)) ENGINE=InnoDB",
		"CREATE TABLE IF NOT EXISTS mydb.`mytable` (`id` int(10)) ENGINE=InnoDB",
		"CREATE TABLE IF NOT EXISTS `mydb`.mytable (`id` int(10)) ENGINE=InnoDB",
	}
	table := "mytable"
	db := "mydb"
	pr := parser.New()
	for _, s := range cases {
		stmts, _, err := pr.Parse(s, "", "")
		if err != nil {
			t.Fatalf("TestCreateTableExp:case %s failed\n", s)
		}
		for _, st := range stmts {
			nodes := parseStmt(st)
			if len(nodes) == 0 {
				continue
			}
			if nodes[0].db != db || nodes[0].table != table {
				t.Fatalf("TestCreateTableExp:case %s failed\n", s)
			}
		}
	}
}
func TestAlterTableExp(t *testing.T) {
	cases := []string{
		"ALTER TABLE /*generated by server*/ `mydb`.`mytable` ADD `field2` DATE  NULL  AFTER `field1`;",
		"ALTER TABLE `mytable` ADD `field2` DATE  NULL  AFTER `field1`;",
		"ALTER TABLE mydb.mytable ADD `field2` DATE  NULL  AFTER `field1`;",
		"ALTER TABLE mytable ADD `field2` DATE  NULL  AFTER `field1`;",
		"ALTER TABLE mydb.mytable ADD field2 DATE  NULL  AFTER `field1`;",
	}

	table := "mytable"
	db := "mydb"
	pr := parser.New()
	for _, s := range cases {
		stmts, _, err := pr.Parse(s, "", "")
		if err != nil {
			t.Fatalf("TestAlterTableExp:case %s failed\n", s)
		}
		for _, st := range stmts {
			nodes := parseStmt(st)
			if len(nodes) == 0 {
				continue
			}
			rdb := nodes[0].db
			rtable := nodes[0].table
			if (len(rdb) > 0 && rdb != db) || rtable != table {
				t.Fatalf("TestAlterTableExp:case %s failed db %s,table %s\n", s, rdb, rtable)
			}
		}
	}
}

func TestRenameTableExp(t *testing.T) {
	cases := []string{
		"rename /* generate by server */table `mydb`.`mytable0` to `mydb`.`mytable0tmp`",
		"rename table `mytable0` to `mytable0tmp`",
		"rename table mydb.mytable0 to mydb.mytable0tmp",
		"rename table mytable0 to mytable0tmp",

		"rename table `mydb`.`mytable0` to `mydb`.`mytable0tmp`, `mydb`.`mytable1` to `mydb`.`mytable1tmp`",
		"rename table `mytable0` to `mytable0tmp`, `mytable1` to `mytable1tmp`",
		"rename table mydb.mytable0 to mydb.mytable0tmp, mydb.mytable1 to mydb.mytable1tmp",
		"rename table mytable0 to mytable0tmp, mytable1 to mytabletmp",
	}
	baseTable := "mytable"
	db := "mydb"
	pr := parser.New()
	for _, s := range cases {
		stmts, _, err := pr.Parse(s, "", "")
		if err != nil {
			t.Fatalf("TestRenameTableExp:case %s failed\n", s)
		}
		for _, st := range stmts {
			nodes := parseStmt(st)
			if len(nodes) == 0 {
				continue
			}
			for i, node := range nodes {
				rdb := node.db
				rtable := node.table
				table := fmt.Sprintf("%s%d", baseTable, i)
				if (len(rdb) > 0 && rdb != db) || rtable != table {
					t.Fatalf("TestRenameTableExp:case %s failed db %s,table %s\n", s, rdb, rtable)
				}
			}
		}
	}
}

func TestDropTableExp(t *testing.T) {
	cases := []string{
		"drop table test0",
		"DROP TABLE test0",
		"DROP TABLE test0",
		"DROP table IF EXISTS test.test0",
		"drop table `test0`",
		"DROP TABLE `test0`",
		"DROP table IF EXISTS `test`.`test0`",
		"DROP TABLE `test0` /* generated by server */",
		"DROP /*generated by server */ table if exists test0",
		"DROP table if exists `test0`",
		"DROP table if exists test.test0",
		"DROP table if exists `test`.test0",
		"DROP table if exists `test`.`test0`",
		"DROP table if exists test.`test0`",
		"DROP table if exists test.`test0`",
	}

	baseTable := "test"
	db := "test"
	pr := parser.New()
	for _, s := range cases {
		stmts, _, err := pr.Parse(s, "", "")
		if err != nil {
			t.Fatalf("TestDropTableExp:case %s failed\n", s)
		}
		for _, st := range stmts {
			nodes := parseStmt(st)
			if len(nodes) == 0 {
				continue
			}
			for i, node := range nodes {
				rdb := node.db
				rtable := node.table
				table := fmt.Sprintf("%s%d", baseTable, i)
				if (len(rdb) > 0 && rdb != db) || rtable != table {
					t.Fatalf("TestDropTableExp:case %s failed db %s,table %s\n", s, rdb, rtable)
				}
			}
		}
	}
}
func TestWithoutSchemeExp(t *testing.T) {

	cases := []replication.QueryEvent{
		replication.QueryEvent{
			Schema: []byte("test"),
			Query:  []byte("drop table test0"),
		},
		replication.QueryEvent{
			Schema: []byte("test"),
			Query:  []byte("rename table `test0` to `testtmp`"),
		},
		replication.QueryEvent{
			Schema: []byte("test"),
			Query:  []byte("ALTER TABLE `test0` ADD `field2` DATE  NULL  AFTER `field1`;"),
		},
		replication.QueryEvent{
			Schema: []byte("test"),
			Query:  []byte("CREATE TABLE IF NOT EXISTS test0 (`id` int(10)) ENGINE=InnoDB"),
		},
	}
	table := "test0"
	db := "test"
	pr := parser.New()
	for _, s := range cases {
		stmts, _, err := pr.Parse(string(s.Query), "", "")
		if err != nil {
			t.Fatalf("TestCreateTableExp:case %s failed\n", s.Query)
		}
		for _, st := range stmts {
			nodes := parseStmt(st)
			if len(nodes) == 0 {
				continue
			}
			if nodes[0].db != "" || nodes[0].table != table || string(s.Schema) != db {
				t.Fatalf("TestCreateTableExp:case %s failed\n", s.Query)
			}
		}
	}
}
