// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bindinfo_test

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"testing"

	. "github.com/pingcap/check"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/auth"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/bindinfo"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/session/txninfo"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/logutil"
	utilparser "github.com/pingcap/tidb/util/parser"
	"github.com/pingcap/tidb/util/testkit"
	"github.com/pingcap/tidb/util/testleak"
	"github.com/tikv/client-go/v2/testutils"
)

func TestT(t *testing.T) {
	CustomVerboseFlag = true
	logLevel := os.Getenv("log_level")
	err := logutil.InitLogger(logutil.NewLogConfig(logLevel, logutil.DefaultLogFormat, "", logutil.EmptyFileLogConfig, false))
	if err != nil {
		t.Fatal(err)
	}
	autoid.SetStep(5000)
	TestingT(t)
}

var _ = Suite(&testSuite{})

type testSuite struct {
	cluster testutils.Cluster
	store   kv.Storage
	domain  *domain.Domain
	*parser.Parser
}

type mockSessionManager struct {
	PS []*util.ProcessInfo
}

func (msm *mockSessionManager) ShowTxnList() []*txninfo.TxnInfo {
	panic("unimplemented!")
}

func (msm *mockSessionManager) ShowProcessList() map[uint64]*util.ProcessInfo {
	ret := make(map[uint64]*util.ProcessInfo)
	for _, item := range msm.PS {
		ret[item.ID] = item
	}
	return ret
}

func (msm *mockSessionManager) GetProcessInfo(id uint64) (*util.ProcessInfo, bool) {
	for _, item := range msm.PS {
		if item.ID == id {
			return item, true
		}
	}
	return &util.ProcessInfo{}, false
}

func (msm *mockSessionManager) Kill(cid uint64, query bool) {
}

func (msm *mockSessionManager) KillAllConnections() {
}

func (msm *mockSessionManager) UpdateTLSConfig(cfg *tls.Config) {
}

func (msm *mockSessionManager) ServerID() uint64 {
	return 1
}

var mockTikv = flag.Bool("mockTikv", true, "use mock tikv store in bind test")

func (s *testSuite) SetUpSuite(c *C) {
	testleak.BeforeTest()
	s.Parser = parser.New()
	flag.Lookup("mockTikv")
	useMockTikv := *mockTikv
	if useMockTikv {
		store, err := mockstore.NewMockStore(
			mockstore.WithClusterInspector(func(c testutils.Cluster) {
				mockstore.BootstrapWithSingleStore(c)
				s.cluster = c
			}),
		)
		c.Assert(err, IsNil)
		s.store = store
		session.SetSchemaLease(0)
		session.DisableStats4Test()
	}
	bindinfo.Lease = 0
	d, err := session.BootstrapSession(s.store)
	c.Assert(err, IsNil)
	d.SetStatsUpdating(true)
	s.domain = d
}

func (s *testSuite) TearDownSuite(c *C) {
	s.domain.Close()
	s.store.Close()
	testleak.AfterTest(c)()
}

func (s *testSuite) TearDownTest(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	r := tk.MustQuery("show tables")
	for _, tb := range r.Rows() {
		tableName := tb[0]
		tk.MustExec(fmt.Sprintf("drop table %v", tableName))
	}
}

func (s *testSuite) cleanBindingEnv(tk *testkit.TestKit) {
	tk.MustExec("delete from mysql.bind_info where source != 'builtin'")
	s.domain.BindHandle().Clear()
}

func normalizeWithDefaultDB(c *C, sql, db string) (string, string) {
	testParser := parser.New()
	stmt, err := testParser.ParseOneStmt(sql, "", "")
	c.Assert(err, IsNil)
	normalized, digest := parser.NormalizeDigest(utilparser.RestoreWithDefaultDB(stmt, "test", ""))
	return normalized, digest.String()
}

var testSQLs = []struct {
	createSQL   string
	overlaySQL  string
	querySQL    string
	originSQL   string
	bindSQL     string
	dropSQL     string
	memoryUsage float64
}{
	{
		createSQL:   "binding for select * from t where i>100 using select * from t use index(index_t) where i>100",
		overlaySQL:  "binding for select * from t where i>99 using select * from t use index(index_t) where i>99",
		querySQL:    "select * from t where i          >      30.0",
		originSQL:   "select * from `test` . `t` where `i` > ?",
		bindSQL:     "SELECT * FROM `test`.`t` USE INDEX (`index_t`) WHERE `i` > 99",
		dropSQL:     "binding for select * from t where i>100",
		memoryUsage: float64(144),
	},
	{
		createSQL:   "binding for select * from t union all select * from t using select * from t use index(index_t) union all select * from t use index()",
		overlaySQL:  "",
		querySQL:    "select * from t union all         select * from t",
		originSQL:   "select * from `test` . `t` union all select * from `test` . `t`",
		bindSQL:     "SELECT * FROM `test`.`t` USE INDEX (`index_t`) UNION ALL SELECT * FROM `test`.`t` USE INDEX ()",
		dropSQL:     "binding for select * from t union all select * from t",
		memoryUsage: float64(200),
	},
	{
		createSQL:   "binding for (select * from t) union all (select * from t) using (select * from t use index(index_t)) union all (select * from t use index())",
		overlaySQL:  "",
		querySQL:    "(select * from t) union all         (select * from t)",
		originSQL:   "( select * from `test` . `t` ) union all ( select * from `test` . `t` )",
		bindSQL:     "(SELECT * FROM `test`.`t` USE INDEX (`index_t`)) UNION ALL (SELECT * FROM `test`.`t` USE INDEX ())",
		dropSQL:     "binding for (select * from t) union all (select * from t)",
		memoryUsage: float64(212),
	},
	{
		createSQL:   "binding for select * from t intersect select * from t using select * from t use index(index_t) intersect select * from t use index()",
		overlaySQL:  "",
		querySQL:    "select * from t intersect         select * from t",
		originSQL:   "select * from `test` . `t` intersect select * from `test` . `t`",
		bindSQL:     "SELECT * FROM `test`.`t` USE INDEX (`index_t`) INTERSECT SELECT * FROM `test`.`t` USE INDEX ()",
		dropSQL:     "binding for select * from t intersect select * from t",
		memoryUsage: float64(200),
	},
	{
		createSQL:   "binding for select * from t except select * from t using select * from t use index(index_t) except select * from t use index()",
		overlaySQL:  "",
		querySQL:    "select * from t except         select * from t",
		originSQL:   "select * from `test` . `t` except select * from `test` . `t`",
		bindSQL:     "SELECT * FROM `test`.`t` USE INDEX (`index_t`) EXCEPT SELECT * FROM `test`.`t` USE INDEX ()",
		dropSQL:     "binding for select * from t except select * from t",
		memoryUsage: float64(194),
	},
	{
		createSQL:   "binding for select * from t using select /*+ use_index(t,index_t)*/ * from t",
		overlaySQL:  "",
		querySQL:    "select * from t ",
		originSQL:   "select * from `test` . `t`",
		bindSQL:     "SELECT /*+ use_index(`t` `index_t`)*/ * FROM `test`.`t`",
		dropSQL:     "binding for select * from t",
		memoryUsage: float64(124),
	},
	{
		createSQL:   "binding for delete from t where i = 1 using delete /*+ use_index(t,index_t) */ from t where i = 1",
		overlaySQL:  "",
		querySQL:    "delete    from t where   i = 2",
		originSQL:   "delete from `test` . `t` where `i` = ?",
		bindSQL:     "DELETE /*+ use_index(`t` `index_t`)*/ FROM `test`.`t` WHERE `i` = 1",
		dropSQL:     "binding for delete from t where i = 1",
		memoryUsage: float64(148),
	},
	{
		createSQL:   "binding for delete t, t1 from t inner join t1 on t.s = t1.s where t.i = 1 using delete /*+ use_index(t,index_t), hash_join(t,t1) */ t, t1 from t inner join t1 on t.s = t1.s where t.i = 1",
		overlaySQL:  "",
		querySQL:    "delete t,   t1 from t inner join t1 on t.s = t1.s  where   t.i = 2",
		originSQL:   "delete `test` . `t` , `test` . `t1` from `test` . `t` join `test` . `t1` on `t` . `s` = `t1` . `s` where `t` . `i` = ?",
		bindSQL:     "DELETE /*+ use_index(`t` `index_t`) hash_join(`t`, `t1`)*/ `test`.`t`,`test`.`t1` FROM `test`.`t` JOIN `test`.`t1` ON `t`.`s` = `t1`.`s` WHERE `t`.`i` = 1",
		dropSQL:     "binding for delete t, t1 from t inner join t1 on t.s = t1.s where t.i = 1",
		memoryUsage: float64(315),
	},
	{
		createSQL:   "binding for update t set s = 'a' where i = 1 using update /*+ use_index(t,index_t) */ t set s = 'a' where i = 1",
		overlaySQL:  "",
		querySQL:    "update   t  set s='b' where i=2",
		originSQL:   "update `test` . `t` set `s` = ? where `i` = ?",
		bindSQL:     "UPDATE /*+ use_index(`t` `index_t`)*/ `test`.`t` SET `s`='a' WHERE `i` = 1",
		dropSQL:     "binding for update t set s = 'a' where i = 1",
		memoryUsage: float64(162),
	},
	{
		createSQL:   "binding for update t, t1 set t.s = 'a' where t.i = t1.i using update /*+ inl_join(t1) */ t, t1 set t.s = 'a' where t.i = t1.i",
		overlaySQL:  "",
		querySQL:    "update   t  , t1 set t.s='b' where t.i=t1.i",
		originSQL:   "update ( `test` . `t` ) join `test` . `t1` set `t` . `s` = ? where `t` . `i` = `t1` . `i`",
		bindSQL:     "UPDATE /*+ inl_join(`t1`)*/ (`test`.`t`) JOIN `test`.`t1` SET `t`.`s`='a' WHERE `t`.`i` = `t1`.`i`",
		dropSQL:     "binding for update t, t1 set t.s = 'a' where t.i = t1.i",
		memoryUsage: float64(230),
	},
	{
		createSQL:   "binding for insert into t1 select * from t where t.i = 1 using insert into t1 select /*+ use_index(t,index_t) */ * from t where t.i = 1",
		overlaySQL:  "",
		querySQL:    "insert  into   t1 select * from t where t.i  = 2",
		originSQL:   "insert into `test` . `t1` select * from `test` . `t` where `t` . `i` = ?",
		bindSQL:     "INSERT INTO `test`.`t1` SELECT /*+ use_index(`t` `index_t`)*/ * FROM `test`.`t` WHERE `t`.`i` = 1",
		dropSQL:     "binding for insert into t1 select * from t where t.i = 1",
		memoryUsage: float64(212),
	},
	{
		createSQL:   "binding for replace into t1 select * from t where t.i = 1 using replace into t1 select /*+ use_index(t,index_t) */ * from t where t.i = 1",
		overlaySQL:  "",
		querySQL:    "replace  into   t1 select * from t where t.i  = 2",
		originSQL:   "replace into `test` . `t1` select * from `test` . `t` where `t` . `i` = ?",
		bindSQL:     "REPLACE INTO `test`.`t1` SELECT /*+ use_index(`t` `index_t`)*/ * FROM `test`.`t` WHERE `t`.`i` = 1",
		dropSQL:     "binding for replace into t1 select * from t where t.i = 1",
		memoryUsage: float64(214),
	},
}

func (s *testSuite) TestExplain(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1(id int)")
	tk.MustExec("create table t2(id int)")

	c.Assert(tk.HasPlan("SELECT * from t1,t2 where t1.id = t2.id", "HashJoin"), IsTrue)
	c.Assert(tk.HasPlan("SELECT  /*+ TIDB_SMJ(t1, t2) */  * from t1,t2 where t1.id = t2.id", "MergeJoin"), IsTrue)

	tk.MustExec("create global binding for SELECT * from t1,t2 where t1.id = t2.id using SELECT  /*+ TIDB_SMJ(t1, t2) */  * from t1,t2 where t1.id = t2.id")

	c.Assert(tk.HasPlan("SELECT * from t1,t2 where t1.id = t2.id", "MergeJoin"), IsTrue)

	tk.MustExec("drop global binding for SELECT * from t1,t2 where t1.id = t2.id")

	// Add test for SetOprStmt
	tk.MustExec("create index index_id on t1(id)")
	c.Assert(tk.HasPlan("SELECT * from t1 union SELECT * from t1", "IndexReader"), IsFalse)
	c.Assert(tk.HasPlan("SELECT * from t1 use index(index_id) union SELECT * from t1", "IndexReader"), IsTrue)

	tk.MustExec("create global binding for SELECT * from t1 union SELECT * from t1 using SELECT * from t1 use index(index_id) union SELECT * from t1")

	c.Assert(tk.HasPlan("SELECT * from t1 union SELECT * from t1", "IndexReader"), IsTrue)

	tk.MustExec("drop global binding for SELECT * from t1 union SELECT * from t1")
}

// TestBindingSymbolList tests sql with "?, ?, ?, ?", fixes #13871
func (s *testSuite) TestBindingSymbolList(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, INDEX ia (a), INDEX ib (b));")
	tk.MustExec("insert into t value(1, 1);")

	// before binding
	tk.MustQuery("select a, b from t where a = 3 limit 1, 100")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ia")
	c.Assert(tk.MustUseIndex("select a, b from t where a = 3 limit 1, 100", "ia(a)"), IsTrue)

	tk.MustExec(`create global binding for select a, b from t where a = 1 limit 0, 1 using select a, b from t use index (ib) where a = 1 limit 0, 1`)

	// after binding
	tk.MustQuery("select a, b from t where a = 3 limit 1, 100")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ib")
	c.Assert(tk.MustUseIndex("select a, b from t where a = 3 limit 1, 100", "ib(b)"), IsTrue)

	// Normalize
	sql, hash := parser.NormalizeDigest("select a, b from test . t where a = 1 limit 0, 1")

	bindData := s.domain.BindHandle().GetBindRecord(hash.String(), sql, "test")
	c.Assert(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select `a` , `b` from `test` . `t` where `a` = ? limit ...")
	bind := bindData.Bindings[0]
	c.Check(bind.BindSQL, Equals, "SELECT `a`,`b` FROM `test`.`t` USE INDEX (`ib`) WHERE `a` = 1 LIMIT 0,1")
	c.Check(bindData.Db, Equals, "test")
	c.Check(bind.Status, Equals, "using")
	c.Check(bind.Charset, NotNil)
	c.Check(bind.Collation, NotNil)
	c.Check(bind.CreateTime, NotNil)
	c.Check(bind.UpdateTime, NotNil)
}

func (s *testSuite) TestDMLSQLBind(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("create table t1(a int, b int, c int, key idx_b(b), key idx_c(c))")
	tk.MustExec("create table t2(a int, b int, c int, key idx_b(b), key idx_c(c))")

	tk.MustExec("delete from t1 where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t1:idx_b")
	c.Assert(tk.MustUseIndex("delete from t1 where b = 1 and c > 1", "idx_b(b)"), IsTrue)
	tk.MustExec("create global binding for delete from t1 where b = 1 and c > 1 using delete /*+ use_index(t1,idx_c) */ from t1 where b = 1 and c > 1")
	tk.MustExec("delete from t1 where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t1:idx_c")
	c.Assert(tk.MustUseIndex("delete from t1 where b = 1 and c > 1", "idx_c(c)"), IsTrue)

	c.Assert(tk.HasPlan("delete t1, t2 from t1 inner join t2 on t1.b = t2.b", "HashJoin"), IsTrue)
	tk.MustExec("create global binding for delete t1, t2 from t1 inner join t2 on t1.b = t2.b using delete /*+ inl_join(t1) */ t1, t2 from t1 inner join t2 on t1.b = t2.b")
	c.Assert(tk.HasPlan("delete t1, t2 from t1 inner join t2 on t1.b = t2.b", "IndexJoin"), IsTrue)

	tk.MustExec("update t1 set a = 1 where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t1:idx_b")
	c.Assert(tk.MustUseIndex("update t1 set a = 1 where b = 1 and c > 1", "idx_b(b)"), IsTrue)
	tk.MustExec("create global binding for update t1 set a = 1 where b = 1 and c > 1 using update /*+ use_index(t1,idx_c) */ t1 set a = 1 where b = 1 and c > 1")
	tk.MustExec("delete from t1 where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t1:idx_c")
	c.Assert(tk.MustUseIndex("update t1 set a = 1 where b = 1 and c > 1", "idx_c(c)"), IsTrue)

	c.Assert(tk.HasPlan("update t1, t2 set t1.a = 1 where t1.b = t2.b", "HashJoin"), IsTrue)
	tk.MustExec("create global binding for update t1, t2 set t1.a = 1 where t1.b = t2.b using update /*+ inl_join(t1) */ t1, t2 set t1.a = 1 where t1.b = t2.b")
	c.Assert(tk.HasPlan("update t1, t2 set t1.a = 1 where t1.b = t2.b", "IndexJoin"), IsTrue)

	tk.MustExec("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t2:idx_b")
	c.Assert(tk.MustUseIndex("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2", "idx_b(b)"), IsTrue)
	tk.MustExec("create global binding for insert into t1 select * from t2 where t2.b = 1 and t2.c > 1 using insert /*+ use_index(t2,idx_c) */ into t1 select * from t2 where t2.b = 1 and t2.c > 1")
	tk.MustExec("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t2:idx_b")
	c.Assert(tk.MustUseIndex("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2", "idx_b(b)"), IsTrue)
	tk.MustExec("drop global binding for insert into t1 select * from t2 where t2.b = 1 and t2.c > 1")
	tk.MustExec("create global binding for insert into t1 select * from t2 where t2.b = 1 and t2.c > 1 using insert into t1 select /*+ use_index(t2,idx_c) */ * from t2 where t2.b = 1 and t2.c > 1")
	tk.MustExec("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t2:idx_c")
	c.Assert(tk.MustUseIndex("insert into t1 select * from t2 where t2.b = 2 and t2.c > 2", "idx_c(c)"), IsTrue)

	tk.MustExec("replace into t1 select * from t2 where t2.b = 2 and t2.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t2:idx_b")
	c.Assert(tk.MustUseIndex("replace into t1 select * from t2 where t2.b = 2 and t2.c > 2", "idx_b(b)"), IsTrue)
	tk.MustExec("create global binding for replace into t1 select * from t2 where t2.b = 1 and t2.c > 1 using replace into t1 select /*+ use_index(t2,idx_c) */ * from t2 where t2.b = 1 and t2.c > 1")
	tk.MustExec("replace into t1 select * from t2 where t2.b = 2 and t2.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t2:idx_c")
	c.Assert(tk.MustUseIndex("replace into t1 select * from t2 where t2.b = 2 and t2.c > 2", "idx_c(c)"), IsTrue)
}

func (s *testSuite) TestBestPlanInBaselines(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, INDEX ia (a), INDEX ib (b));")
	tk.MustExec("insert into t value(1, 1);")

	// before binding
	tk.MustQuery("select a, b from t where a = 3 limit 1, 100")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ia")
	c.Assert(tk.MustUseIndex("select a, b from t where a = 3 limit 1, 100", "ia(a)"), IsTrue)

	tk.MustQuery("select a, b from t where b = 3 limit 1, 100")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ib")
	c.Assert(tk.MustUseIndex("select a, b from t where b = 3 limit 1, 100", "ib(b)"), IsTrue)

	tk.MustExec(`create global binding for select a, b from t where a = 1 limit 0, 1 using select /*+ use_index(@sel_1 test.t ia) */ a, b from t where a = 1 limit 0, 1`)
	tk.MustExec(`create global binding for select a, b from t where b = 1 limit 0, 1 using select /*+ use_index(@sel_1 test.t ib) */ a, b from t where b = 1 limit 0, 1`)

	sql, hash := normalizeWithDefaultDB(c, "select a, b from t where a = 1 limit 0, 1", "test")
	bindData := s.domain.BindHandle().GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select `a` , `b` from `test` . `t` where `a` = ? limit ...")
	bind := bindData.Bindings[0]
	c.Check(bind.BindSQL, Equals, "SELECT /*+ use_index(@`sel_1` `test`.`t` `ia`)*/ `a`,`b` FROM `test`.`t` WHERE `a` = 1 LIMIT 0,1")
	c.Check(bindData.Db, Equals, "test")
	c.Check(bind.Status, Equals, "using")

	tk.MustQuery("select a, b from t where a = 3 limit 1, 10")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ia")
	c.Assert(tk.MustUseIndex("select a, b from t where a = 3 limit 1, 100", "ia(a)"), IsTrue)

	tk.MustQuery("select a, b from t where b = 3 limit 1, 100")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:ib")
	c.Assert(tk.MustUseIndex("select a, b from t where b = 3 limit 1, 100", "ib(b)"), IsTrue)
}

func (s *testSuite) TestErrorBind(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustGetErrMsg("create global binding for select * from t using select * from t", "[schema:1146]Table 'test.t' doesn't exist")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t(i int, s varchar(20))")
	tk.MustExec("create table t1(i int, s varchar(20))")
	tk.MustExec("create index index_t on t(i,s)")

	_, err := tk.Exec("create global binding for select * from t where i>100 using select * from t use index(index_t) where i>100")
	c.Assert(err, IsNil, Commentf("err %v", err))

	sql, hash := parser.NormalizeDigest("select * from test . t where i > ?")
	bindData := s.domain.BindHandle().GetBindRecord(hash.String(), sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `i` > ?")
	bind := bindData.Bindings[0]
	c.Check(bind.BindSQL, Equals, "SELECT * FROM `test`.`t` USE INDEX (`index_t`) WHERE `i` > 100")
	c.Check(bindData.Db, Equals, "test")
	c.Check(bind.Status, Equals, "using")
	c.Check(bind.Charset, NotNil)
	c.Check(bind.Collation, NotNil)
	c.Check(bind.CreateTime, NotNil)
	c.Check(bind.UpdateTime, NotNil)

	tk.MustExec("drop index index_t on t")
	_, err = tk.Exec("select * from t where i > 10")
	c.Check(err, IsNil)

	s.domain.BindHandle().DropInvalidBindRecord()

	rs, err := tk.Exec("show global bindings")
	c.Assert(err, IsNil)
	chk := rs.NewChunk()
	err = rs.Next(context.TODO(), chk)
	c.Check(err, IsNil)
	c.Check(chk.NumRows(), Equals, 0)
}

func (s *testSuite) TestDMLEvolveBaselines(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, c int, index idx_b(b), index idx_c(c))")
	tk.MustExec("insert into t values (1,1,1), (2,2,2), (3,3,3), (4,4,4), (5,5,5)")
	tk.MustExec("analyze table t")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")

	tk.MustExec("create global binding for delete from t where b = 1 and c > 1 using delete /*+ use_index(t,idx_c) */ from t where b = 1 and c > 1")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	tk.MustExec("delete /*+ use_index(t,idx_b) */ from t where b = 2 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_c")
	tk.MustExec("admin flush bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	tk.MustExec("admin evolve bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)

	tk.MustExec("create global binding for update t set a = 1 where b = 1 and c > 1 using update /*+ use_index(t,idx_c) */ t set a = 1 where b = 1 and c > 1")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	tk.MustExec("update /*+ use_index(t,idx_b) */ t set a = 2 where b = 2 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_c")
	tk.MustExec("admin flush bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	tk.MustExec("admin evolve bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)

	tk.MustExec("create table t1 like t")
	tk.MustExec("create global binding for insert into t1 select * from t where t.b = 1 and t.c > 1 using insert into t1 select /*+ use_index(t,idx_c) */ * from t where t.b = 1 and t.c > 1")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 3)
	tk.MustExec("insert into t1 select /*+ use_index(t,idx_b) */ * from t where t.b = 2 and t.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_c")
	tk.MustExec("admin flush bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 3)
	tk.MustExec("admin evolve bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 3)

	tk.MustExec("create global binding for replace into t1 select * from t where t.b = 1 and t.c > 1 using replace into t1 select /*+ use_index(t,idx_c) */ * from t where t.b = 1 and t.c > 1")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 4)
	tk.MustExec("replace into t1 select /*+ use_index(t,idx_b) */ * from t where t.b = 2 and t.c > 2")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_c")
	tk.MustExec("admin flush bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 4)
	tk.MustExec("admin evolve bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 4)
}

func (s *testSuite) TestAddEvolveTasks(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, c int, index idx_a(a), index idx_b(b), index idx_c(c))")
	tk.MustExec("insert into t values (1,1,1), (2,2,2), (3,3,3), (4,4,4), (5,5,5)")
	tk.MustExec("analyze table t")
	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 and c = 0 using select * from t use index(idx_a) where a >= 1 and b >= 1 and c = 0")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")
	// It cannot choose table path although it has lowest cost.
	tk.MustQuery("select * from t where a >= 4 and b >= 1 and c = 0")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_a")
	tk.MustExec("admin flush bindings")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	c.Assert(rows[0][1], Equals, "SELECT /*+ use_index(@`sel_1` `test`.`t` )*/ * FROM `test`.`t` WHERE `a` >= 4 AND `b` >= 1 AND `c` = 0")
	c.Assert(rows[0][3], Equals, "pending verify")
	tk.MustExec("admin evolve bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	c.Assert(rows[0][1], Equals, "SELECT /*+ use_index(@`sel_1` `test`.`t` )*/ * FROM `test`.`t` WHERE `a` >= 4 AND `b` >= 1 AND `c` = 0")
	status := rows[0][3].(string)
	c.Assert(status == "using" || status == "rejected", IsTrue)
}

func (s *testSuite) TestRuntimeHintsInEvolveTasks(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")
	tk.MustExec("create table t(a int, b int, c int, index idx_a(a), index idx_b(b), index idx_c(c))")

	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 and c = 0 using select * from t use index(idx_a) where a >= 1 and b >= 1 and c = 0")
	tk.MustQuery("select /*+ MAX_EXECUTION_TIME(5000) */ * from t where a >= 4 and b >= 1 and c = 0")
	tk.MustExec("admin flush bindings")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	c.Assert(rows[0][1], Equals, "SELECT /*+ use_index(@`sel_1` `test`.`t` `idx_c`), max_execution_time(5000)*/ * FROM `test`.`t` WHERE `a` >= 4 AND `b` >= 1 AND `c` = 0")
}

func (s *testSuite) TestDefaultSessionVars(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustQuery(`show variables like "%baselines%"`).Sort().Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
		"tidb_evolve_plan_baselines OFF",
		"tidb_use_plan_baselines ON"))
	tk.MustQuery(`show global variables like "%baselines%"`).Sort().Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
		"tidb_evolve_plan_baselines OFF",
		"tidb_use_plan_baselines ON"))
}

func (s *testSuite) TestCaptureBaselinesScope(c *C) {
	tk1 := testkit.NewTestKit(c, s.store)
	tk2 := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk1)
	tk1.MustQuery(`show session variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
	))
	tk1.MustQuery(`show global variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
	))
	tk1.MustQuery(`select @@session.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"0",
	))
	tk1.MustQuery(`select @@global.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"0",
	))

	tk1.MustExec("set @@session.tidb_capture_plan_baselines = on")
	defer func() {
		tk1.MustExec(" set @@session.tidb_capture_plan_baselines = off")
	}()
	tk1.MustQuery(`show session variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines ON",
	))
	tk1.MustQuery(`show global variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
	))
	tk1.MustQuery(`select @@session.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"1",
	))
	tk1.MustQuery(`select @@global.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"0",
	))
	tk2.MustQuery(`show session variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines ON",
	))
	tk2.MustQuery(`show global variables like "tidb_capture_plan_baselines"`).Check(testkit.Rows(
		"tidb_capture_plan_baselines OFF",
	))
	tk2.MustQuery(`select @@session.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"1",
	))
	tk2.MustQuery(`select @@global.tidb_capture_plan_baselines`).Check(testkit.Rows(
		"0",
	))
}

func (s *testSuite) TestStmtHints(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, index idx(a))")
	tk.MustExec("create global binding for select * from t using select /*+ MAX_EXECUTION_TIME(100), MEMORY_QUOTA(1 GB) */ * from t use index(idx)")
	tk.MustQuery("select * from t")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.MemQuotaQuery, Equals, int64(1073741824))
	c.Assert(tk.Se.GetSessionVars().StmtCtx.MaxExecutionTime, Equals, uint64(100))
	tk.MustQuery("select a, b from t")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.MemQuotaQuery, Equals, int64(0))
	c.Assert(tk.Se.GetSessionVars().StmtCtx.MaxExecutionTime, Equals, uint64(0))
}

func (s *testSuite) TestPrivileges(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, index idx(a))")
	tk.MustExec("create global binding for select * from t using select * from t use index(idx)")
	c.Assert(tk.Se.Auth(&auth.UserIdentity{Username: "root", Hostname: "%"}, nil, nil), IsTrue)
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	tk.MustExec("create user test@'%'")
	c.Assert(tk.Se.Auth(&auth.UserIdentity{Username: "test", Hostname: "%"}, nil, nil), IsTrue)
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 0)
}

func (s *testSuite) TestHintsSetEvolveTask(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, index idx_a(a))")
	tk.MustExec("create global binding for select * from t where a > 10 using select * from t ignore index(idx_a) where a > 10")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")
	tk.MustQuery("select * from t use index(idx_a) where a > 0")
	bindHandle := s.domain.BindHandle()
	bindHandle.SaveEvolveTasksToStore()
	// Verify the added Binding for evolution contains valid ID and Hint, otherwise, panic may happen.
	sql, hash := normalizeWithDefaultDB(c, "select * from t where a > ?", "test")
	bindData := bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 2)
	bind := bindData.Bindings[1]
	c.Assert(bind.Status, Equals, bindinfo.PendingVerify)
	c.Assert(bind.ID, Not(Equals), "")
	c.Assert(bind.Hint, NotNil)
}

func (s *testSuite) TestHintsSetID(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, index idx_a(a))")
	tk.MustExec("create global binding for select * from t where a > 10 using select /*+ use_index(test.t, idx_a) */ * from t where a > 10")
	bindHandle := s.domain.BindHandle()
	// Verify the added Binding contains ID with restored query block.
	sql, hash := normalizeWithDefaultDB(c, "select * from t where a > ?", "test")
	bindData := bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind := bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "use_index(@`sel_1` `test`.`t` `idx_a`)")

	s.cleanBindingEnv(tk)
	tk.MustExec("create global binding for select * from t where a > 10 using select /*+ use_index(t, idx_a) */ * from t where a > 10")
	bindData = bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind = bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "use_index(@`sel_1` `test`.`t` `idx_a`)")

	s.cleanBindingEnv(tk)
	tk.MustExec("create global binding for select * from t where a > 10 using select /*+ use_index(@sel_1 t, idx_a) */ * from t where a > 10")
	bindData = bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind = bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "use_index(@`sel_1` `test`.`t` `idx_a`)")

	s.cleanBindingEnv(tk)
	tk.MustExec("create global binding for select * from t where a > 10 using select /*+ use_index(@qb1 t, idx_a) qb_name(qb1) */ * from t where a > 10")
	bindData = bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind = bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "use_index(@`sel_1` `test`.`t` `idx_a`)")

	s.cleanBindingEnv(tk)
	tk.MustExec("create global binding for select * from t where a > 10 using select /*+ use_index(T, IDX_A) */ * from t where a > 10")
	bindData = bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind = bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "use_index(@`sel_1` `test`.`t` `idx_a`)")

	s.cleanBindingEnv(tk)
	err := tk.ExecToErr("create global binding for select * from t using select /*+ non_exist_hint() */ * from t")
	c.Assert(terror.ErrorEqual(err, parser.ErrWarnOptimizerHintParseError), IsTrue)
	tk.MustExec("create global binding for select * from t where a > 10 using select * from t where a > 10")
	bindData = bindHandle.GetBindRecord(hash, sql, "test")
	c.Check(bindData, NotNil)
	c.Check(bindData.OriginalSQL, Equals, "select * from `test` . `t` where `a` > ?")
	c.Assert(len(bindData.Bindings), Equals, 1)
	bind = bindData.Bindings[0]
	c.Assert(bind.ID, Equals, "")
}

func (s *testSuite) TestNotEvolvePlanForReadStorageHint(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, index idx_a(a), index idx_b(b))")
	tk.MustExec("insert into t values (1,1), (2,2), (3,3), (4,4), (5,5), (6,6), (7,7), (8,8), (9,9), (10,10)")
	tk.MustExec("analyze table t")
	// Create virtual tiflash replica info.
	dom := domain.GetDomain(tk.Se)
	is := dom.InfoSchema()
	db, exists := is.SchemaByName(model.NewCIStr("test"))
	c.Assert(exists, IsTrue)
	for _, tblInfo := range db.Tables {
		if tblInfo.Name.L == "t" {
			tblInfo.TiFlashReplica = &model.TiFlashReplicaInfo{
				Count:     1,
				Available: true,
			}
		}
	}

	// Make sure the best plan of the SQL is use TiKV index.
	tk.MustExec("set @@session.tidb_executor_concurrency = 4;")
	rows := tk.MustQuery("explain select * from t where a >= 11 and b >= 11").Rows()
	c.Assert(fmt.Sprintf("%v", rows[len(rows)-1][2]), Equals, "cop[tikv]")

	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 using select /*+ read_from_storage(tiflash[t]) */ * from t where a >= 1 and b >= 1")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")

	// Even if index of TiKV has lower cost, it chooses TiFlash.
	rows = tk.MustQuery("explain select * from t where a >= 11 and b >= 11").Rows()
	c.Assert(fmt.Sprintf("%v", rows[len(rows)-1][2]), Equals, "cop[tiflash]")

	tk.MustExec("admin flush bindings")
	rows = tk.MustQuery("show global bindings").Rows()
	// None evolve task, because of the origin binding is a read_from_storage binding.
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][1], Equals, "SELECT /*+ read_from_storage(tiflash[`t`])*/ * FROM `test`.`t` WHERE `a` >= 1 AND `b` >= 1")
	c.Assert(rows[0][3], Equals, "using")
}

func (s *testSuite) TestBindingWithIsolationRead(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, index idx_a(a), index idx_b(b))")
	tk.MustExec("insert into t values (1,1), (2,2), (3,3), (4,4), (5,5), (6,6), (7,7), (8,8), (9,9), (10,10)")
	tk.MustExec("analyze table t")
	// Create virtual tiflash replica info.
	dom := domain.GetDomain(tk.Se)
	is := dom.InfoSchema()
	db, exists := is.SchemaByName(model.NewCIStr("test"))
	c.Assert(exists, IsTrue)
	for _, tblInfo := range db.Tables {
		if tblInfo.Name.L == "t" {
			tblInfo.TiFlashReplica = &model.TiFlashReplicaInfo{
				Count:     1,
				Available: true,
			}
		}
	}
	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 using select * from t use index(idx_a) where a >= 1 and b >= 1")
	tk.MustExec("set @@tidb_use_plan_baselines = 1")
	rows := tk.MustQuery("explain select * from t where a >= 11 and b >= 11").Rows()
	c.Assert(rows[len(rows)-1][2], Equals, "cop[tikv]")
	// Even if we build a binding use index for SQL, but after we set the isolation read for TiFlash, it choose TiFlash instead of index of TiKV.
	tk.MustExec("set @@tidb_isolation_read_engines = \"tiflash\"")
	rows = tk.MustQuery("explain select * from t where a >= 11 and b >= 11").Rows()
	c.Assert(rows[len(rows)-1][2], Equals, "cop[tiflash]")
}

func (s *testSuite) TestReCreateBindAfterEvolvePlan(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = true
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, c int, index idx_a(a), index idx_b(b), index idx_c(c))")
	tk.MustExec("insert into t values (1,1,1), (2,2,2), (3,3,3), (4,4,4), (5,5,5)")
	tk.MustExec("analyze table t")
	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 using select * from t use index(idx_a) where a >= 1 and b >= 1")
	tk.MustExec("set @@tidb_evolve_plan_baselines=1")

	// It cannot choose table path although it has lowest cost.
	tk.MustQuery("select * from t where a >= 0 and b >= 0")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_a")

	tk.MustExec("admin flush bindings")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 2)
	c.Assert(rows[0][1], Equals, "SELECT /*+ use_index(@`sel_1` `test`.`t` )*/ * FROM `test`.`t` WHERE `a` >= 0 AND `b` >= 0")
	c.Assert(rows[0][3], Equals, "pending verify")

	tk.MustExec("create global binding for select * from t where a >= 1 and b >= 1 using select * from t use index(idx_b) where a >= 1 and b >= 1")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	tk.MustQuery("select * from t where a >= 4 and b >= 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_b")
}

func (s *testSuite) TestInvisibleIndex(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, unique idx_a(a), index idx_b(b) invisible)")
	tk.MustGetErrMsg(
		"create global binding for select * from t using select * from t use index(idx_b) ",
		"[planner:1176]Key 'idx_b' doesn't exist in table 't'")

	// Create bind using index
	tk.MustExec("create global binding for select * from t using select * from t use index(idx_a) ")

	tk.MustQuery("select * from t")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_a")
	c.Assert(tk.MustUseIndex("select * from t", "idx_a(a)"), IsTrue)

	tk.MustExec(`prepare stmt1 from 'select * from t'`)
	tk.MustExec("execute stmt1")
	c.Assert(len(tk.Se.GetSessionVars().StmtCtx.IndexNames), Equals, 1)
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_a")

	// And then make this index invisible
	tk.MustExec("alter table t alter index idx_a invisible")
	tk.MustQuery("select * from t")
	c.Assert(len(tk.Se.GetSessionVars().StmtCtx.IndexNames), Equals, 0)

	tk.MustExec("execute stmt1")
	c.Assert(len(tk.Se.GetSessionVars().StmtCtx.IndexNames), Equals, 0)

	tk.MustExec("drop binding for select * from t")
}

func (s *testSuite) TestSPMHitInfo(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1(id int)")
	tk.MustExec("create table t2(id int)")

	c.Assert(tk.HasPlan("SELECT * from t1,t2 where t1.id = t2.id", "HashJoin"), IsTrue)
	c.Assert(tk.HasPlan("SELECT  /*+ TIDB_SMJ(t1, t2) */  * from t1,t2 where t1.id = t2.id", "MergeJoin"), IsTrue)

	tk.MustExec("SELECT * from t1,t2 where t1.id = t2.id")
	tk.MustQuery(`select @@last_plan_from_binding;`).Check(testkit.Rows("0"))
	tk.MustExec("create global binding for SELECT * from t1,t2 where t1.id = t2.id using SELECT  /*+ TIDB_SMJ(t1, t2) */  * from t1,t2 where t1.id = t2.id")

	c.Assert(tk.HasPlan("SELECT * from t1,t2 where t1.id = t2.id", "MergeJoin"), IsTrue)
	tk.MustExec("SELECT * from t1,t2 where t1.id = t2.id")
	tk.MustQuery(`select @@last_plan_from_binding;`).Check(testkit.Rows("1"))
	tk.MustExec("drop global binding for SELECT * from t1,t2 where t1.id = t2.id")
}

func (s *testSuite) TestReCreateBind(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, index idx(a))")

	tk.MustQuery("select * from mysql.bind_info where source != 'builtin'").Check(testkit.Rows())
	tk.MustQuery("show global bindings").Check(testkit.Rows())

	tk.MustExec("create global binding for select * from t using select * from t")
	tk.MustQuery("select original_sql, status from mysql.bind_info where source != 'builtin';").Check(testkit.Rows(
		"select * from `test` . `t` using",
	))
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][0], Equals, "select * from `test` . `t`")
	c.Assert(rows[0][3], Equals, "using")

	tk.MustExec("create global binding for select * from t using select * from t")
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][0], Equals, "select * from `test` . `t`")
	c.Assert(rows[0][3], Equals, "using")

	rows = tk.MustQuery("select original_sql, status from mysql.bind_info where source != 'builtin';").Rows()
	c.Assert(len(rows), Equals, 2)
	c.Assert(rows[0][1], Equals, "deleted")
	c.Assert(rows[1][1], Equals, "using")
}

func (s *testSuite) TestExplainShowBindSQL(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, key(a))")

	tk.MustExec("create global binding for select * from t using select * from t use index(a)")
	tk.MustQuery("select original_sql, bind_sql from mysql.bind_info where default_db != 'mysql'").Check(testkit.Rows(
		"select * from `test` . `t` SELECT * FROM `test`.`t` USE INDEX (`a`)",
	))

	tk.MustExec("explain format = 'verbose' select * from t")
	tk.MustQuery("show warnings").Check(testkit.Rows("Note 1105 Using the bindSQL: SELECT * FROM `test`.`t` USE INDEX (`a`)"))
	// explain analyze do not support verbose yet.
}

func (s *testSuite) TestDMLIndexHintBind(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("create table t(a int, b int, c int, key idx_b(b), key idx_c(c))")

	tk.MustExec("delete from t where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_b")
	c.Assert(tk.MustUseIndex("delete from t where b = 1 and c > 1", "idx_b(b)"), IsTrue)
	tk.MustExec("create global binding for delete from t where b = 1 and c > 1 using delete from t use index(idx_c) where b = 1 and c > 1")
	tk.MustExec("delete from t where b = 1 and c > 1")
	c.Assert(tk.Se.GetSessionVars().StmtCtx.IndexNames[0], Equals, "t:idx_c")
	c.Assert(tk.MustUseIndex("delete from t where b = 1 and c > 1", "idx_c(c)"), IsTrue)
}

func (s *testSuite) TestForbidEvolvePlanBaseLinesBeforeGA(c *C) {
	originalVal := config.CheckTableBeforeDrop
	config.CheckTableBeforeDrop = false
	defer func() {
		config.CheckTableBeforeDrop = originalVal
	}()

	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	err := tk.ExecToErr("set @@tidb_evolve_plan_baselines=0")
	c.Assert(err, Equals, nil)
	err = tk.ExecToErr("set @@TiDB_Evolve_pLan_baselines=1")
	c.Assert(err, ErrorMatches, "Cannot enable baseline evolution feature, it is not generally available now")
	err = tk.ExecToErr("set @@TiDB_Evolve_pLan_baselines=oN")
	c.Assert(err, ErrorMatches, "Cannot enable baseline evolution feature, it is not generally available now")
	err = tk.ExecToErr("admin evolve bindings")
	c.Assert(err, ErrorMatches, "Cannot enable baseline evolution feature, it is not generally available now")
}

func (s *testSuite) TestExplainTableStmts(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(id int, value decimal(5,2))")
	tk.MustExec("table t")
	tk.MustExec("explain table t")
	tk.MustExec("desc table t")
}

func (s *testSuite) TestSPMWithoutUseDatabase(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk1 := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	s.cleanBindingEnv(tk1)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, key(a))")
	tk.MustExec("create global binding for select * from t using select * from t force index(a)")

	err := tk1.ExecToErr("select * from t")
	c.Assert(err, ErrorMatches, "*No database selected")
	tk1.MustQuery(`select @@last_plan_from_binding;`).Check(testkit.Rows("0"))
	c.Assert(tk1.MustUseIndex("select * from test.t", "a"), IsTrue)
	tk1.MustExec("select * from test.t")
	tk1.MustQuery(`select @@last_plan_from_binding;`).Check(testkit.Rows("1"))
}

func (s *testSuite) TestBindingWithoutCharset(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a varchar(10) CHARACTER SET utf8)")
	tk.MustExec("create global binding for select * from t where a = 'aa' using select * from t where a = 'aa'")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][0], Equals, "select * from `test` . `t` where `a` = ?")
	c.Assert(rows[0][1], Equals, "SELECT * FROM `test`.`t` WHERE `a` = 'aa'")
}

func (s *testSuite) TestGCBindRecord(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	s.cleanBindingEnv(tk)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int, key(a))")

	tk.MustExec("create global binding for select * from t where a = 1 using select * from t use index(a) where a = 1")
	rows := tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][0], Equals, "select * from `test` . `t` where `a` = ?")
	c.Assert(rows[0][3], Equals, "using")
	tk.MustQuery("select status from mysql.bind_info where original_sql = 'select * from `test` . `t` where `a` = ?'").Check(testkit.Rows(
		"using",
	))

	h := s.domain.BindHandle()
	// bindinfo.Lease is set to 0 for test env in SetUpSuite.
	c.Assert(h.GCBindRecord(), IsNil)
	rows = tk.MustQuery("show global bindings").Rows()
	c.Assert(len(rows), Equals, 1)
	c.Assert(rows[0][0], Equals, "select * from `test` . `t` where `a` = ?")
	c.Assert(rows[0][3], Equals, "using")
	tk.MustQuery("select status from mysql.bind_info where original_sql = 'select * from `test` . `t` where `a` = ?'").Check(testkit.Rows(
		"using",
	))

	tk.MustExec("drop global binding for select * from t where a = 1")
	tk.MustQuery("show global bindings").Check(testkit.Rows())
	tk.MustQuery("select status from mysql.bind_info where original_sql = 'select * from `test` . `t` where `a` = ?'").Check(testkit.Rows(
		"deleted",
	))
	c.Assert(h.GCBindRecord(), IsNil)
	tk.MustQuery("show global bindings").Check(testkit.Rows())
	tk.MustQuery("select status from mysql.bind_info where original_sql = 'select * from `test` . `t` where `a` = ?'").Check(testkit.Rows())
}
