# MStorm

[English README](README.md)

MStorm 是 [storm](https://github.com/asdine/storm) 的一个分支，基于 [BoltDB/bbolt](https://github.com/etcd-io/bbolt) 提供类型化 CRUD、键值存储、查询器和索引能力。

当前版本将业务数据保存在 BoltDB 中，并使用 [Bleve](https://github.com/blevesearch/bleve) 在数据库文件同级目录维护外部索引，支持普通索引、唯一索引、复合索引和全文索引。

## 安装

```bash
GO111MODULE=on go get -u github.com/clakeboy/storm-rev
```

## 导入

```go
import storm "github.com/clakeboy/storm-rev"
```

## 打开数据库

```go
db, err := storm.Open("my.db")
if err != nil {
  return err
}
defer db.Close()
```

`Open` 支持传入多个选项，例如自定义 bbolt 参数、Codec、已有 Bolt 连接、批量写入模式等。

## 定义模型

```go
type User struct {
  ID    int    // 主键。没有显式 storm:"id" 时，名为 ID 的字段会作为主键。
  Group string `storm:"index,composite=group_age:1"` // 普通索引，并作为复合索引第一列。
  Email string `storm:"unique"`                      // 唯一索引。
  Name  string                                       // 不建索引。
  Age   int    `storm:"index,composite=group_age:2"` // 普通索引，并作为复合索引第二列。
  Bio   string `storm:"fulltext"`                    // 全文索引。
}
```

支持的 `storm` 标签：

- `id`：主键字段。
- `index`：普通索引，支持 `Find`、`One`、`AllByIndex`、`Range`、`Prefix`。
- `unique`：唯一索引，保存时会检查唯一性。
- `fulltext`：全文索引，通过 `Search` 查询。
- `composite=name:order`：复合索引字段，`order` 必须从 `1` 开始且连续。
- `increment`：整数自增字段，可写成 `increment=100` 指定起始值。
- `inline`：读取嵌入结构体中的标签。

复合索引可以和普通索引共用同一个字段：

```go
type User struct {
  ID    int
  Group string `storm:"index,composite=group_age:1"`
  Age   int    `storm:"index,composite=group_age:2"`
}
```

## 保存数据

```go
user := User{
  ID:    10,
  Group: "staff",
  Email: "john@provider.com",
  Name:  "John",
  Age:   21,
  Bio:   "John works on search features",
}

err := db.Save(&user)
```

`Save` 会创建 bucket，更新索引，检查唯一约束，并把对象写入 BoltDB。

大量导入数据时可以使用 `SaveAll`。它会在一个 BoltDB 写事务里校验并写入整个切片，然后批量更新外部索引。

```go
users := []User{
  {ID: 1, Name: "John"},
  {ID: 2, Name: "Jane"},
}

err := db.SaveAll(users)
```

## 自增字段

```go
type Product struct {
  ID                  int    `storm:"id,increment"`
  Name                string
  IndexedIntegerField uint32 `storm:"index,increment"`
  UniqueIntegerField  int16  `storm:"unique,increment=100"`
}

p := Product{Name: "Vacuum Cleaner"}
err := db.Save(&p)
```

保存后，自增字段会被写回结构体。

## 查询

### 查询单条记录

```go
var user User
err := db.One("Email", "john@provider.com", &user)
```

### 查询多条记录

```go
var users []User
err := db.Find("Group", "staff", &users)
```

### 查询全部记录

```go
var users []User
err := db.All(&users)
```

### 按索引顺序查询全部记录

```go
var users []User
err := db.AllByIndex("Age", &users)
```

### 范围查询

```go
var users []User
err := db.Range("Age", 18, 30, &users)
```

### 前缀查询

```go
var users []User
err := db.Prefix("Name", "Jo", &users)
```

### 复合索引查询

`FindByIndex` 用索引名和完整字段值列表查询。当前复合索引只支持完整等值匹配。

```go
var users []User
err := db.FindByIndex("group_age", []any{"staff", 21}, &users)
```

### 全文搜索

`fulltext` 字段使用 Bleve 分词索引，并通过 `Search` 查询。`Find` 仍保持精确匹配语义。

```go
type Article struct {
  ID    int
  Title string `storm:"fulltext"`
}

var articles []Article
err := db.Search("Title", "bleve search", &articles)
```

### Skip、Limit、Reverse

```go
var users []User

err := db.Find("Group", "staff", &users, storm.Skip(10))
err = db.Find("Group", "staff", &users, storm.Limit(10))
err = db.Find("Group", "staff", &users, storm.Reverse())
err = db.Find("Group", "staff", &users, storm.Limit(10), storm.Skip(10), storm.Reverse())

err = db.AllByIndex("Age", &users, storm.Limit(10), storm.Skip(10), storm.Reverse())
err = db.Range("Age", 18, 30, &users, storm.Limit(10), storm.Skip(10), storm.Reverse())
err = db.Search("Title", "bleve", &articles, storm.Limit(10), storm.Skip(10), storm.Reverse())
```

## 更新和删除

```go
// 更新非零值字段。
err := db.Update(&User{ID: 10, Name: "Jack", Age: 45})

// 更新单个字段，支持零值。
err = db.UpdateField(&User{ID: 10}, "Age", 0)

// 删除结构体对应记录。
err = db.DeleteStruct(&User{ID: 10})
```

更新和删除会同步维护外部 Bleve 索引。

## 初始化、删除和重建索引

### 初始化 bucket 和索引

```go
err := db.Init(&User{})
```

### 删除 bucket

```go
err := db.Drop(&User{})
err = db.Drop("User")
```

`Drop` 会删除 Bolt bucket，并清理对应的 Bleve 索引目录。

### 重建索引

```go
err := db.ReIndex(&User{})
```

当结构体标签变更、索引目录丢失或索引需要恢复时，使用 `ReIndex` 重建指定表的索引。`ReIndex` 是唯一公开的索引重建入口。

## 索引文件

MStorm 把数据保存在 BoltDB 中，把索引作为可重建的外部 Bleve 索引保存。

如果数据库路径是：

```text
/path/app.db
```

索引根目录是：

```text
/path/app_db_index/
```

每个表一个独立索引目录，例如：

```text
/path/app_db_index/User.bleve
```

BoltDB 始终是事实数据源。如果外部索引不一致，可以通过 `ReIndex` 从 BoltDB 全量重建。

## 高级查询

可以通过 `Select` 和 `q` 包组合更复杂的条件。`Select` 不依赖索引，会扫描 bucket 中的记录。

```go
import "github.com/clakeboy/storm-rev/q"

var users []User
err := db.Select(
  q.Gte("Age", 18),
  q.Lte("Age", 30),
  q.Eq("Group", "staff"),
).Find(&users)
```

常用 matcher：

```go
q.Eq("Name", "John")
q.StrictEq("Age", 21)
q.Gt("Age", 18)
q.Gte("Age", 18)
q.Lt("Age", 30)
q.Lte("Age", 30)
q.Re("Name", "^J")
q.In("Group", []string{"staff", "admin"})
q.And(q.Gt("Age", 18), q.Eq("Group", "staff"))
q.Or(q.Eq("Group", "staff"), q.Eq("Group", "admin"))
q.Not(q.Eq("Group", "guest"))
```

`Query` 支持分页、排序、删除和逐条处理：

```go
query := db.Select(q.Gte("Age", 18)).Limit(10).Skip(20).OrderBy("Age").Reverse()

err := query.Find(&users)

err = query.Each(new(User), func(record interface{}) error {
  user := record.(*User)
  _ = user
  return nil
})

err = query.Delete(new(User))
```

## 事务

```go
tx, err := db.Begin(true)
if err != nil {
  return err
}
defer tx.Rollback()

err = tx.Save(&User{ID: 1, Group: "staff"})
if err != nil {
  return err
}

return tx.Commit()
```

事务内的索引查询会回退到 Bolt 扫描。事务提交后，受影响表的 Bleve 索引会重建。

## 配置选项

### BoltOptions

```go
db, err := storm.Open("my.db", storm.BoltOptions(0600, &bolt.Options{
  Timeout: time.Second,
}))
```

### Codec

默认使用 JSON 编码。可以通过 `storm.Codec` 使用其他编码器：

```go
db, err := storm.Open("my.db", storm.Codec(gob.Codec))
```

内置 Codec：

- JSON：`github.com/clakeboy/storm-rev/codec/json`
- GOB：`github.com/clakeboy/storm-rev/codec/gob`
- Sereal：`github.com/clakeboy/storm-rev/codec/sereal`
- Protocol Buffers：`github.com/clakeboy/storm-rev/codec/protobuf`
- MessagePack：`github.com/clakeboy/storm-rev/codec/msgpack`

### UseDB

可以把已有的 BoltDB 连接交给 MStorm：

```go
bDB, err := bolt.Open("bolt.db", 0600, &bolt.Options{Timeout: 10 * time.Second})
db, err := storm.Open("ignored.db", storm.UseDB(bDB))
```

使用 `UseDB` 时，索引目录会根据传入 BoltDB 的文件名创建，并把文件名里的 `.` 替换成 `_`，例如 `bolt.db` 对应 `bolt_db_index`。

### Batch

```go
db, err := storm.Open("my.db", storm.Batch())
```

## 嵌套 bucket

`From` 可以创建相对某个嵌套 bucket 的节点，并复用同一套 API。

```go
repo := db.From("repo")

err := repo.Save(&Issue{
  ID:     1,
  Title:  "Need more features",
  Author: user.ID,
})

var issues []Issue
err = repo.Find("Author", user.ID, &issues)
```

可以继续链式嵌套：

```go
items := db.From("items")
potions := items.From("consumables").From("medicine").From("potions")
_ = potions
```

## 简单键值存储

MStorm 也可以作为简单的 key/value 存储使用。

```go
err := db.Set("sessions", "session-id", &user)

var loaded User
err = db.Get("sessions", "session-id", &loaded)

err = db.Delete("sessions", "session-id")
```

## 直接访问 BoltDB

底层 BoltDB 仍然可以直接访问。

```go
err := db.Bolt.View(func(tx *bolt.Tx) error {
  bucket := tx.Bucket([]byte("User"))
  if bucket == nil {
    return nil
  }
  value := bucket.Get([]byte("some-id"))
  _ = value
  return nil
})
```

也可以把 Bolt 事务传给 MStorm 节点：

```go
err := db.Bolt.Update(func(tx *bolt.Tx) error {
  node := db.WithTransaction(tx)
  return node.Save(&user)
})
```

## License

MIT

## Credits

- [Asdine El Hrychy](https://github.com/asdine)
- [Bjørn Erik Pedersen](https://github.com/bep)
