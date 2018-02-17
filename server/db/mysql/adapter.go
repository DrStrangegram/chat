// +build mysql

package mysql

import (
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	ms "github.com/go-sql-driver/mysql"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"
)

// adapter holds RethinkDb connection data.
type adapter struct {
	db      *sql.DB
	dbName  string
	version int
}

const (
	defaultDSN      = "root:@tcp(localhost:3306)/tinode"
	defaultDatabase = "tinode"

	dbVersion = 100
)

type configType struct {
	DSN string `json:"dsn,omitempty"`
}

const (
	// Maximum number of records to return
	maxResults = 1024
	// Maximum number of topic subscribers to return
	maxSubscribers = 256
)

// Open initializes rethinkdb session
func (a *adapter) Open(jsonconfig string) error {
	if a.db != nil {
		return errors.New("mysql adapter is already connected")
	}

	var err error
	var config configType

	if err = json.Unmarshal([]byte(jsonconfig), &config); err != nil {
		return errors.New("mysql adapter failed to parse config: " + err.Error())
	}

	dsn := config.DSN
	if dsn == "" {
		dsn = defaultDSN
	}

	a.db, err = sql.Open("mysql", dsn)
	if err != nil {
		return err
	}

	// sql.Open does not open the network connection.
	// Force network connection here.
	err = a.db.Ping()
	if err != nil {
		return err
	}

	a.version = -1

	return nil
}

// Close closes the underlying database connection
func (a *adapter) Close() error {
	var err error
	if a.db != nil {
		err = a.db.Close()
		a.db = nil
		a.version = -1
	}
	return err
}

// IsOpen returns true if connection to database has been established. It does not check if
// connection is actually live.
func (a *adapter) IsOpen() bool {
	return a.db != nil
}

// Read current database version
func (a *adapter) getDbVersion() (int, error) {
	resp := a.db.QueryRow("SELECT `value` FROM kvmeta WHERE `key`='version'")

	var vers map[string]int
	if err := resp.Scan(&vers); err != nil {
		return -1, err
	}
	a.version = vers["value"]

	return a.version, nil
}

// CheckDbVersion checks whether the actual DB version matches the expected version of this adapter.
func (a *adapter) CheckDbVersion() error {
	if a.version <= 0 {
		a.getDbVersion()
	}

	if a.version != dbVersion {
		return errors.New("Invalid database version " + strconv.Itoa(a.version) +
			". Expected " + strconv.Itoa(dbVersion))
	}

	return nil
}

// CreateDb initializes the storage. Unsupported in the adapter: use external script schema.sql
func (a *adapter) CreateDb(reset bool) error {
	return errors.New("unsupported: use schema.sql to create database")
}

// Indexable tag as stored in 'tagunique'
type storedTag struct {
	Id     string
	Source string
}

// UserCreate creates a new user. Returns error and true if error is due to duplicate user name,
// false for any other error
func (a *adapter) UserCreate(user *t.User) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Save user's tags to a separate table to ensure uniquness
	if len(user.Tags) > 0 {
		tags := make([]storedTag, 0, len(user.Tags))
		for _, t := range user.Tags {
			tags = append(tags, storedTag{Id: t, Source: user.Id})
		}
		_, err = a.db.Exec("INSERT INTO tagunique() VALUES()", tags)
		if err != nil {
			if isDupe(err) {
				err = errors.New("duplicate tag(s)")
			}
			return err
		}
	}

	_, err = a.db.Exec("INSERT INTO users() VALUES()")
	if err != nil {
		return err
	}

	return tx.Commit()
}

// Add user's authentication record
func (a *adapter) AddAuthRecord(uid t.Uid, authLvl int, unique string,
	secret []byte, expires time.Time) (bool, error) {

	_, err := a.db.Exec("INSERT INTO auth(`unique`,userid,authLvl,secret,expires) VALUES(?,?,?,?,?)",
		unique, uid.String(), authLvl, secret)
	if err != nil {
		if isDupe(err) {
			return true, errors.New("duplicate credential")
		}
		return false, err
	}
	return false, nil
}

// Delete user's authentication record
func (a *adapter) DelAuthRecord(unique string) (int, error) {
	res, err := a.db.Exec("DELETE FROM auth WHERE `unique`=?", unique)
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()

	return int(count), nil
}

// Delete user's all authentication records
func (a *adapter) DelAllAuthRecords(uid t.Uid) (int, error) {
	res, err := a.db.Exec("DELETE FROM auth WHERE userid=?", uid)
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()

	return int(count), nil
}

// Update user's authentication secret
func (a *adapter) UpdAuthRecord(unique string, authLvl int, secret []byte, expires time.Time) (int, error) {
	res, err := a.db.Exec("UPDATE auth SET authLvl=?,secret=?,expires=? WHERE `unique`=?",
		authLvl, secret, expires, unique)

	if err != nil {
		return 0, err
	}

	count, _ := res.RowsAffected()
	return int(count), nil
}

// Retrieve user's authentication record
func (a *adapter) GetAuthRecord(unique string) (t.Uid, int, []byte, time.Time, error) {
	res := a.db.QueryRow("SELECT userid, secret, expires, authLvl FROM auth WHERE `unique`=?", unique)

	var record struct {
		Userid  string
		AuthLvl int
		Secret  []byte
		Expires time.Time
	}

	if err := res.Scan(&record); err != nil {
		return t.ZeroUid, 0, nil, time.Time{}, err
	}

	// log.Println("loggin in user Id=", user.Uid(), user.Id)
	return t.ParseUid(record.Userid), record.AuthLvl, record.Secret, record.Expires, nil
}

// UserGet fetches a single user by user id. If user is not found it returns (nil, nil)
func (a *adapter) UserGet(uid t.Uid) (*t.User, error) {
	res := a.db.QueryRow("SELECT * FROM users WHERE id=?", uid)
	var user t.User
	var err error
	if err = res.Scan(&user); err == nil {
		return &user, nil
	} else if err == sql.ErrNoRows {
		// Clear the error if user does not exist
		err = nil
	}

	// If user does not exist, it returns nil, nil
	return nil, err
}

func (a *adapter) UserGetAll(ids ...t.Uid) ([]t.User, error) {
	uids := make([]interface{}, len(ids))
	for i, id := range ids {
		uids[i] = id.String()
	}

	users := []t.User{}
	if rows, err := a.db.Query("SELECT * FROM users WHERE id IN (?)", uids); err == nil {
		defer rows.Close()

		var user t.User
		for err = rows.Scan(&user); err == nil; {
			users = append(users, user)
		}

		if err != nil {
			return nil, err
		}

	} else {
		return nil, err
	}

	return users, nil
}

func (a *adapter) UserDelete(uid t.Uid, soft bool) error {
	var err error
	if soft {
		now := t.TimeNow()
		_, err = a.db.Exec("UPDATE users set updatedAt=?, deletedAt=? WHERE id=?", now, now, uid)
	} else {
		_, err = a.db.Exec("DELETE FROM users WHERE id=?", uid)
	}
	return err
}

func (a *adapter) UserUpdateLastSeen(uid t.Uid, userAgent string, when time.Time) error {
	_, err := a.db.Exec("UPDATE users SET lastseen=?, useragent=? WHERE id=?", when, userAgent, uid)

	return err
}

// UserUpdate updates user object. Use UserTagsUpdate when updating Tags.
func (a *adapter) UserUpdate(uid t.Uid, update map[string]interface{}) error {
	_, err := a.db.Exec("UPDATE users SET var=? WHERE id=?", update, uid)
	return err
}

// *****************************

// TopicCreate creates a topic from template
func (a *adapter) TopicCreate(topic *t.Topic) error {
	_, err := a.db.Exec("INSERT INTO topics(columns) VALUES(values)", topic)
	return err
}

// TopicCreateP2P given two users creates a p2p topic
func (a *adapter) TopicCreateP2P(initiator, invited *t.Subscription) error {
	initiator.Id = initiator.Topic + ":" + initiator.User
	// Don't care if the initiator changes own subscription
	_, err := a.db.Exec("INSERT INTO subscriptions(id) VALUES(?) "+
		"ON DUPLICATE KEY UPDATE ", initiator)
	if err != nil {
		return err
	}

	// Ensure this is a new subscription. If one already exist, don't overwrite it
	invited.Id = invited.Topic + ":" + invited.User
	_, err = a.db.Exec("INSERT INTO subscriptions(id) VALUES(?)", invited)
	if err != nil {
		// Is this a duplicate subscription? If so, ifnore it. Otherwise it's a genuine DB error
		if isDupe(err) {
			return err
		}
	}

	topic := &t.Topic{ObjHeader: t.ObjHeader{Id: initiator.Topic}}
	topic.ObjHeader.MergeTimes(&initiator.ObjHeader)
	return a.TopicCreate(topic)
}

func (a *adapter) TopicGet(topic string) (*t.Topic, error) {
	// Fetch topic by name
	row := a.db.QueryRow("SELECT * FROM topics WHERE name=?", topic)

	var tt = new(t.Topic)
	if err := row.Scan(tt); err != nil {
		return nil, err
	}

	return tt, nil
}

// TopicsForUser loads user's contact list: p2p and grp topics, except for 'me' subscription.
// Reads and denormalizes Public value.
func (a *adapter) TopicsForUser(uid t.Uid, keepDeleted bool) ([]t.Subscription, error) {
	// Fetch user's subscriptions
	// Subscription have Topic.UpdatedAt denormalized into Subscription.UpdatedAt
	q := "SELECT * FROM subscriptions WHERE userid=?"

	if !keepDeleted {
		// Filter out rows with defined DeletedAt
		q += " AND deletedAt IS NULL"
	}
	q += "LIMIT ?"

	//log.Printf("RethinkDbAdapter.TopicsForUser q: %+v", q)

	rows, err := a.db.Query(q, uid, maxResults)
	if err != nil {
		return nil, err
	}

	// Fetch subscriptions. Two queries are needed: users table (me & p2p) and topics table (p2p and grp).
	// Prepare a list of Separate subscriptions to users vs topics
	var sub t.Subscription
	join := make(map[string]t.Subscription) // Keeping these to make a join with table for .private and .access
	topq := make([]interface{}, 0, 16)
	usrq := make([]interface{}, 0, 16)
	for err = rows.Scan(&sub); err == nil; {
		tcat := t.GetTopicCat(sub.Topic)

		// 'me' or 'fnd' subscription, skip
		if tcat == t.TopicCatMe || tcat == t.TopicCatFnd {
			continue

			// p2p subscription, find the other user to get user.Public
		} else if tcat == t.TopicCatP2P {
			uid1, uid2, _ := t.ParseP2P(sub.Topic)
			if uid1 == uid {
				usrq = append(usrq, uid2.String())
			} else {
				usrq = append(usrq, uid1.String())
			}
			topq = append(topq, sub.Topic)

			// grp subscription
		} else {
			topq = append(topq, sub.Topic)
		}
		join[sub.Topic] = sub
	}
	rows.Close()

	//log.Printf("RethinkDbAdapter.TopicsForUser topq, usrq: %+v, %+v", topq, usrq)
	var subs []t.Subscription
	if len(topq) > 0 || len(usrq) > 0 {
		subs = make([]t.Subscription, 0, len(join))
	}

	if len(topq) > 0 {
		// Fetch grp & p2p topics
		rows, err = a.db.Query("SELECT * FROM topics WHERE name IN(?)", topq...)
		if err != nil {
			return nil, err
		}

		var top t.Topic
		for err = rows.Scan(&top); err == nil; {
			sub = join[top.Id]
			sub.ObjHeader.MergeTimes(&top.ObjHeader)
			sub.SetSeqId(top.SeqId)
			// sub.SetDelId(top.DelId)
			if t.GetTopicCat(sub.Topic) == t.TopicCatGrp {
				// all done with a grp topic
				sub.SetPublic(top.Public)
				subs = append(subs, sub)
			} else {
				// put back the updated value of a p2p subsription, will process further below
				join[top.Id] = sub
			}
		}
		rows.Close()
		//log.Printf("RethinkDbAdapter.TopicsForUser 1: %#+v", subs)
	}

	// Fetch p2p users and join to p2p tables
	if len(usrq) > 0 {
		rows, err = a.db.Query("SELECT * FROM users WHERE id IN (?)", usrq...)
		if err != nil {
			return nil, err
		}

		var usr t.User
		for err = rows.Scan(&usr); err == nil; {
			uid2 := t.ParseUid(usr.Id)
			topic := uid.P2PName(uid2)
			if sub, ok := join[topic]; ok {
				sub.ObjHeader.MergeTimes(&usr.ObjHeader)
				sub.SetPublic(usr.Public)
				sub.SetWith(uid2.UserId())
				sub.SetDefaultAccess(usr.Access.Auth, usr.Access.Anon)
				sub.SetLastSeenAndUA(usr.LastSeen, usr.UserAgent)
				subs = append(subs, sub)
			}
		}
		rows.Close()

		//log.Printf("RethinkDbAdapter.TopicsForUser 2: %#+v", subs)
	}

	return subs, nil
}

// UsersForTopic loads users subscribed to the given topic
func (a *adapter) UsersForTopic(topic string, keepDeleted bool) ([]t.Subscription, error) {
	// Fetch all subscribed users. The number of users is not large
	q := "SELECT * FROM subscriptions WHERE topic=?"
	if !keepDeleted {
		// Filter out rows with DeletedAt being not null
		q += " AND deletedAt IS NULL"
	}
	q += " LIMIT ?"
	//log.Printf("RethinkDbAdapter.UsersForTopic q: %+v", q)
	rows, err := a.db.Query(q, topic, maxSubscribers)
	if err != nil {
		return nil, err
	}

	// Fetch subscriptions
	var sub t.Subscription
	var subs []t.Subscription
	join := make(map[string]t.Subscription)
	usrq := make([]interface{}, 0, 16)
	for err = rows.Scan(&sub); err == nil; {
		join[sub.User] = sub
		usrq = append(usrq, sub.User)
	}
	rows.Close()

	//log.Printf("RethinkDbAdapter.UsersForTopic usrq: %+v, usrq)
	if len(usrq) > 0 {
		subs = make([]t.Subscription, 0, len(usrq))

		// Fetch users by a list of subscriptions
		rows, err = a.db.Query("SELECT * FROM users WHERE id IN (?)", usrq...)
		if err != nil {
			return nil, err
		}

		var usr t.User
		for rows.Next(); err == nil; err = rows.Scan(&usr) {
			if sub, ok := join[usr.Id]; ok {
				sub.ObjHeader.MergeTimes(&usr.ObjHeader)
				sub.SetPublic(usr.Public)
				subs = append(subs, sub)
			}
		}
		rows.Close()

		//log.Printf("RethinkDbAdapter.UsersForTopic users: %+v", subs)
	}

	return subs, nil
}

func (a *adapter) TopicShare(shares []*t.Subscription) (int, error) {
	// Assign Ids.
	for i := 0; i < len(shares); i++ {
		shares[i].Id = shares[i].Topic + ":" + shares[i].User
	}
	// Subscription could have been marked as deleted (DeletedAt != nil). If it's marked
	// as deleted, unmark.
	resp, err := a.db.Exec("INSERT INTO subscriptions() VALUES() ON DUPLICATE KEY UPDATE", shares)

	if err != nil {
		return 0, err
	}

	count, err := resp.RowsAffected()
	return int(count), err
}

func (a *adapter) TopicDelete(topic string) error {
	_, err := a.db.Exec("DELETE FROM topics WHERE name=?", topic)
	return err
}

func (a *adapter) TopicUpdateOnMessage(topic string, msg *t.Message) error {
	_, err := a.db.Exec("UPDATE topics SET seqID=? WHERE name=?", msg.SeqId, topic)

	return err
}

func (a *adapter) TopicUpdate(topic string, update map[string]interface{}) error {
	_, err := a.db.Exec("UPDATE topics SET ? WHERE name=?", update, topic)
	return err
}

// Get a subscription of a user to a topic
func (a *adapter) SubscriptionGet(topic string, user t.Uid) (*t.Subscription, error) {

	row := a.db.QueryRow("SELECT * FROM subscriptions WHERE id=?", topic+":"+user.String())

	var sub t.Subscription
	if err := row.Scan(&sub); err != nil {
		return nil, err
	}

	if sub.DeletedAt != nil {
		return nil, nil
	}
	return &sub, nil
}

// Update time when the user was last attached to the topic
func (a *adapter) SubsLastSeen(topic string, user t.Uid, lastSeen map[string]time.Time) error {
	_, err := a.db.Exec("UPDATE subscriptions SET lastseen=? WHERE id=?", lastSeen, topic+":"+user.String())

	return err
}

// SubsForUser loads a list of user's subscriptions to topics. Does NOT read Public value.
func (a *adapter) SubsForUser(forUser t.Uid, keepDeleted bool) ([]t.Subscription, error) {
	if forUser.IsZero() {
		return nil, errors.New("mysql adapter: invalid user ID in SubsForUser")
	}

	q := "SELECT * FROM subscriptions WHERE user=?"
	if !keepDeleted {
		q += " AND deletedAt IS NULL"
	}
	q += " LIMIT ?"

	rows, err := a.db.Query(q, forUser, maxResults)
	if err != nil {
		return nil, err
	}

	var subs []t.Subscription
	var ss t.Subscription
	for err = rows.Scan(&ss); err == nil; {
		subs = append(subs, ss)
	}
	return subs, rows.Err()
}

// SubsForTopic fetches all subsciptions for a topic.
func (a *adapter) SubsForTopic(topic string, keepDeleted bool) ([]t.Subscription, error) {
	//log.Println("Loading subscriptions for topic ", topic)

	// must load User.Public for p2p topics
	var p2p []t.User
	var err error
	if t.GetTopicCat(topic) == t.TopicCatP2P {
		uid1, uid2, _ := t.ParseP2P(topic)
		if p2p, err = a.UserGetAll(uid1, uid2); err != nil {
			return nil, err
		} else if len(p2p) != 2 {
			return nil, errors.New("failed to load two p2p users")
		}
	}

	q := "SELECT * FROM subscriptions WHERE topic=?"
	if !keepDeleted {
		// Filter out rows where DeletedAt is defined
		q += " AND deletedAt IS NULL"
	}
	q += " LIMIT ?"
	//log.Println("Loading subscription q=", q)

	rows, err := a.db.Query(q, topic, maxSubscribers)
	if err != nil {
		return nil, err
	}

	var subs []t.Subscription
	var ss t.Subscription
	for err = rows.Scan(&ss); err == nil; {
		if p2p != nil {
			// Assigning values provided by the other user
			if p2p[0].Id == ss.User {
				ss.SetPublic(p2p[1].Public)
				ss.SetWith(p2p[1].Id)
				ss.SetDefaultAccess(p2p[1].Access.Auth, p2p[1].Access.Anon)
			} else {
				ss.SetPublic(p2p[0].Public)
				ss.SetWith(p2p[0].Id)
				ss.SetDefaultAccess(p2p[0].Access.Auth, p2p[0].Access.Anon)
			}
		}
		subs = append(subs, ss)
		//log.Printf("SubsForTopic: loaded sub %#+v", ss)
	}
	rows.Close()

	return subs, err
}

// SubsUpdate updates a single subscription.
func (a *adapter) SubsUpdate(topic string, user t.Uid, update map[string]interface{}) error {
	q := "UPDATE subscriptions SET ? WHERE "
	var param interface{}
	if !user.IsZero() {
		// Update one topic subscription
		q += "user=?"
		param = user
	} else {
		// Update all topic subscriptions
		q += "topic=?"
		param = topic
	}
	_, err := a.db.Exec(q, update, param)
	return err
}

// SubsDelete marks subscription as deleted.
func (a *adapter) SubsDelete(topic string, user t.Uid) error {
	now := t.TimeNow()
	_, err := a.db.Exec("UPDATE subscriptions SET updatedAt=?, deletedAT=? WHERE id=?",
		now, now, topic+":"+user.String())
	return err
}

// SubsDelForTopic marks all subscriptions to the given topic as deleted
func (a *adapter) SubsDelForTopic(topic string) error {
	now := t.TimeNow()
	_, err := a.db.Exec("UPDATE subscriptions SET updatedAt=?, deletedAt=? WHERE topic=?", now, now, topic)
	return err
}

// Returns a list of users who match given tags, such as "email:jdoe@example.com" or "tel:18003287448".
// Searching the 'users.Tags' for the given tags using respective index.
func (a *adapter) FindUsers(uid t.Uid, tags []string) ([]t.Subscription, error) {
	index := make(map[string]struct{})
	var query []interface{}
	for _, tag := range tags {
		query = append(query, tag)
		index[tag] = struct{}{}
	}

	// Get users matched by tags, sort by number of matches from high to low.
	// Use JOIN users  -> tags
	rows, err := a.db.Query("SELECT id, access, createdAt, updatedAt, public, tags "+
		"FROM users WHERE tags IN (?) GROUP BY id ORDER BY matchCount LIMIT ?",
		query, maxResults)

	if err != nil {
		return nil, err
	}

	var user t.User
	var sub t.Subscription
	var subs []t.Subscription
	for err = rows.Scan(&user); err == nil; {
		if user.Id == uid.String() {
			// Skip the callee
			continue
		}
		sub.CreatedAt = user.CreatedAt
		sub.UpdatedAt = user.UpdatedAt
		sub.User = user.Id
		sub.SetPublic(user.Public)
		// TODO: maybe report default access to user
		// sub.SetDefaultAccess(user.Access.Auth, user.Access.Anon)
		tags := make([]string, 0, 1)
		for _, tag := range user.Tags {
			if _, ok := index[tag]; ok {
				tags = append(tags, tag)
			}
		}
		sub.Private = tags
		subs = append(subs, sub)
	}
	rows.Close()

	return subs, err

}

// Returns a list of topics with matching tags.
// Searching the 'topics.Tags' for the given tags using respective index.
func (a *adapter) FindTopics(tags []string) ([]t.Subscription, error) {
	index := make(map[string]struct{})
	var query []interface{}
	for _, tag := range tags {
		query = append(query, tag)
		index[tag] = struct{}{}
	}

	rows, err := rdb.DB(a.dbName).
		Table("topics").
		GetAllByIndex("Tags", query...).
		Pluck("Id", "Access", "CreatedAt", "UpdatedAt", "Public", "Tags").
		Group("Id").
		Ungroup().
		Map(func(row rdb.Term) rdb.Term {
			return row.Field("reduction").
				Nth(0).
				Merge(map[string]interface{}{"MatchedTagsCount": row.Field("reduction").Count()})
		}).
		OrderBy(rdb.Desc("MatchedTagsCount")).
		Limit(maxResults).
		Run(a.conn)
	if err != nil {
		return nil, err
	}

	var topic t.Topic
	var sub t.Subscription
	var subs []t.Subscription
	for rows.Next(&topic) {
		sub.CreatedAt = topic.CreatedAt
		sub.UpdatedAt = topic.UpdatedAt
		sub.Topic = topic.Id
		sub.SetPublic(topic.Public)
		// TODO: maybe report default access to user
		// sub.SetDefaultAccess(user.Access.Auth, user.Access.Anon)
		tags := make([]string, 0, 1)
		for _, tag := range topic.Tags {
			if _, ok := index[tag]; ok {
				tags = append(tags, tag)
			}
		}
		sub.Private = tags
		subs = append(subs, sub)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}
	return subs, nil

}

// UserTagsUpdate updates user's Tags. 'unique' contains the prefixes of tags which are
// treated as unique, i.e. 'email' or 'tel'.
func (a *adapter) UserTagsUpdate(uid t.Uid, unique, tags []string) error {
	user, err := a.UserGet(uid)
	if err != nil {
		return err
	}

	added, removed := tagsUniqueDelta(unique, user.Tags, tags)
	if err := a.updateUniqueTags(user.Id, added, removed); err != nil {
		return err
	}

	return a.UserUpdate(uid, map[string]interface{}{"Tags": tags})
}

// TopicTagsUpdate updates topic's tags.
// - name is the name of the topic to update
// - unique is the list of prefixes to treat as unique.
// - tags are the new tags.
func (a *adapter) TopicTagsUpdate(name string, unique, tags []string) error {
	topic, err := a.TopicGet(name)
	if err != nil {
		return err
	}

	added, removed := tagsUniqueDelta(unique, topic.Tags, tags)
	if err := a.updateUniqueTags(name, added, removed); err != nil {
		return err
	}

	return a.TopicUpdate(name, map[string]interface{}{"Tags": tags})
}

func (a *adapter) updateUniqueTags(source string, added, removed []string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if added != nil && len(added) > 0 {
		toAdd := make([]storedTag, 0, len(added))
		for _, tag := range added {
			toAdd = append(toAdd, storedTag{Id: tag, Source: source})
		}

		_, err = tx.Exec("INSERT INTO tagunique() VALUES(?)", toAdd)
		if err != nil {
			if isDupe(err) {
				return errors.New("duplicate tag(s)")
			}
			return err
		}
	}

	if removed != nil && len(removed) > 0 {
		_, err = a.db.Exec("DELETE FROM tagunique WHERE tag IN (?) AND source=?", removed, source)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// tagsUniqueDelta extracts the lists of added unique tags and removed unique tags:
//   added :=  newTags - (oldTags & newTags) -- present in new but missing in old
//   removed := oldTags - (newTags & oldTags) -- present in old but missing in new
func tagsUniqueDelta(unique, oldTags, newTags []string) (added, removed []string) {
	if oldTags == nil {
		return filterUniqueTags(unique, newTags), nil
	}
	if newTags == nil {
		return nil, filterUniqueTags(unique, oldTags)
	}

	sort.Strings(oldTags)
	sort.Strings(newTags)

	// Match old tags against the new tags and separate removed tags from added.
	iold, inew := 0, 0
	lold, lnew := len(oldTags), len(newTags)
	for iold < lold || inew < lnew {
		if (iold == lold && inew < lnew) || oldTags[iold] > newTags[inew] {
			// Present in new, missing in old: added
			added = append(added, newTags[inew])
			inew++

		} else if (inew == lnew && iold < lold) || oldTags[iold] < newTags[inew] {
			// Present in old, missing in new: removed
			removed = append(removed, oldTags[iold])

			iold++

		} else {
			// present in both
			if iold < lold {
				iold++
			}
			if inew < lnew {
				inew++
			}
		}
	}
	return filterUniqueTags(unique, added), filterUniqueTags(unique, removed)
}

func filterUniqueTags(unique, tags []string) []string {
	var out []string
	if unique != nil && len(unique) > 0 && tags != nil {
		for _, s := range tags {
			parts := strings.SplitN(s, ":", 2)

			if len(parts) < 2 {
				continue
			}

			for _, u := range unique {
				if parts[0] == u {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Messages
func (a *adapter) MessageSave(msg *t.Message) error {
	msg.SetUid(store.GetUid())
	_, err := a.db.Exec("INSERT INTO messages() VALUES(?)", msg)
	return err
}

func (a *adapter) MessageGetAll(topic string, forUser t.Uid, opts *t.BrowseOpt) ([]t.Message, error) {
	//log.Println("Loading messages for topic ", topic, opts)

	var limit = maxResults // TODO(gene): pass into adapter as a config param
	var lower = 0
	var upper = 1 << 31

	if opts != nil {
		if opts.Since > 0 {
			lower = opts.Since
		}
		if opts.Before > 0 {
			upper = opts.Before
		}

		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	rows, err := a.db.Query("SELECT * FROM messages WHERE topic=? AND seqid BETWEEN ? AND ? "+
		"AND delid IS NULL AND filter-soft-deleted-for-current-user "+
		"ORDER BY seqid DESC LIMIT ?", topic, lower, upper, forUser, limit)

	if err != nil {
		return nil, err
	}

	var msgs []t.Message
	if err = rows.Scan(&msgs); err != nil {
		return nil, err
	}

	return msgs, nil
}

// Get ranges of deleted messages
func (a *adapter) MessageGetDeleted(topic string, forUser t.Uid, opts *t.BrowseOpt) ([]t.DelMessage, error) {
	var limit = maxResults
	var lower = 0
	var upper = 1 << 31

	if opts != nil {
		if opts.Since > 0 {
			lower = opts.Since
		}
		if opts.Before > 0 {
			upper = opts.Before
		}

		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	// Fetch log of deletions
	rows, err := a.db.Query("SELECT * FROM dellog WHERE topic=? AND delid BETWEEN ? and ? "+
		"AND (deletedFor IS NULL OR deletedFor=?)"+
		"ORDER BY delid LIMIT ?", topic, lower, upper, forUser, limit)
	if err != nil {
		return nil, err
	}

	var dmsgs []t.DelMessage
	if err = rows.Scan(&dmsgs); err != nil {
		return nil, err
	}

	return dmsgs, nil
}

// MessageDeleteList deletes messages in the given topic with seqIds from the list
func (a *adapter) MessageDeleteList(topic string, toDel *t.DelMessage) (err error) {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if toDel == nil {
		// Whole topic is being deleted, thus also deleting all messages
		_, err = a.db.Exec("DELETE FROM messages WHERE topic=?", topic)
	} else {
		// Only some messages are being deleted
		toDel.SetUid(store.GetUid())

		// Start with making a log entry
		_, err = a.db.Exec("INSERT INTO dellog() VALUES(?)", toDel)
		if err != nil {
			return err
		}

		if len(toDel.SeqIdRanges) > 1 || toDel.SeqIdRanges[0].Hi <= toDel.SeqIdRanges[0].Low {
			for _, rng := range toDel.SeqIdRanges {
				if rng.Hi == 0 {
					indexVals = append(indexVals, []interface{}{topic, rng.Low})
				} else {
					for i := rng.Low; i <= rng.Hi; i++ {
						indexVals = append(indexVals, []interface{}{topic, i})
					}
				}
			}
			query = query.GetAllByIndex("Topic_SeqId", indexVals...)
		} else {
			// Optimizing for a special case of single range low..hi
			query = query.Between(
				[]interface{}{topic, toDel.SeqIdRanges[0].Low},
				[]interface{}{topic, toDel.SeqIdRanges[0].Hi},
				rdb.BetweenOpts{Index: "Topic_SeqId", RightBound: "closed"})
		}

		if toDel.DeletedFor == "" {
			// Hard-deleting for all users{
			// Hard-delete of individual messages. Mark some messages as deleted.
			_, err = query.Filter(rdb.Row.HasFields("DelId").Not()).
				Update(map[string]interface{}{"DeletedAt": t.TimeNow(), "DelId": toDel.DelId, "From": nil,
					"Head": nil, "Content": nil}).RunWrite(a.conn)
		} else {
			// Soft-deleting: adding DelId to DeletedFor
			_, err = query.
				// Skip hard-deleted messages
				Filter(rdb.Row.HasFields("DelId").Not()).
				// Skip messages already soft-deleted for the current user
				Filter(func(row rdb.Term) interface{} {
					return rdb.Not(row.Field("DeletedFor").Default([]interface{}{}).Contains(
						func(df rdb.Term) interface{} {
							return df.Field("User").Eq(toDel.DeletedFor)
						}))
				}).Update(map[string]interface{}{"DeletedFor": rdb.Row.Field("DeletedFor").
				Default([]interface{}{}).Append(
				&t.SoftDelete{
					User:  toDel.DeletedFor,
					DelId: toDel.DelId})}).RunWrite(a.conn)
		}
	}

	if err != nil {
		return err
	}

	return tx.Commit()
}

func deviceHasher(deviceID string) string {
	// Generate custom key as [64-bit hash of device id] to ensure predictable
	// length of the key
	hasher := fnv.New64()
	hasher.Write([]byte(deviceID))
	return strconv.FormatUint(uint64(hasher.Sum64()), 16)
}

// Device management for push notifications
func (a *adapter) DeviceUpsert(uid t.Uid, def *t.DeviceDef) error {
	hash := deviceHasher(def.DeviceId)
	user := uid.String()

	// Ensure uniqueness of the device ID
	var others []interface{}
	// Find users who already use this device ID, ignore current user.
	if resp, err := rdb.DB(a.dbName).Table("users").GetAllByIndex("DeviceIds", def.DeviceId).
		// We only care about user Ids
		Pluck("Id").
		// Make sure we filter out the current user who may legitimately use this device ID
		Filter(rdb.Not(rdb.Row.Field("Id").Eq(user))).
		// Convert slice of objects to a slice of strings
		ConcatMap(func(row rdb.Term) interface{} { return []interface{}{row.Field("Id")} }).
		// Execute
		Run(a.conn); err != nil {
		return err
	} else if err = resp.All(&others); err != nil {
		return err
	} else if len(others) > 0 {
		// Delete device ID for the other users.
		_, err := rdb.DB(a.dbName).Table("users").GetAll(others...).Replace(rdb.Row.Without(
			map[string]string{"Devices": hash})).RunWrite(a.conn)
		if err != nil {
			return err
		}
	}

	// Actually add/update DeviceId for the new user
	_, err := rdb.DB(a.dbName).Table("users").Get(user).
		Update(map[string]interface{}{
			"Devices": map[string]*t.DeviceDef{
				hash: def,
			}}).RunWrite(a.conn)
	return err
}

func (a *adapter) DeviceGetAll(uids ...t.Uid) (map[t.Uid][]t.DeviceDef, int, error) {

	rows, err := a.db.Query("SELECT userid, deviceID FROM devices WHERE userid=?", uids)
	if err != nil {
		return nil, 0, err
	}

	var row struct {
		Id      string
		Devices map[string]*t.DeviceDef
	}

	result := make(map[t.Uid][]t.DeviceDef)
	count := 0
	var uid t.Uid
	for err = rows.Scan(&row); err == nil; {
		if row.Devices != nil && len(row.Devices) > 0 {
			if err := uid.UnmarshalText([]byte(row.Id)); err != nil {
				continue
			}

			result[uid] = make([]t.DeviceDef, len(row.Devices))
			i := 0
			for _, def := range row.Devices {
				if def != nil {
					result[uid][i] = *def
					i++
					count++
				}
			}
		}
	}

	return result, count, rows.Err()
}

func (a *adapter) DeviceDelete(uid t.Uid, deviceID string) error {
	_, err := a.db.Exec("DELETE FROM devices WHERE userid=? AND hash=?", uid, deviceHasher(deviceID))
	return err
}

func isDupe(err error) bool {
	myerr, ok := err.(*ms.MySQLError)
	return ok && myerr.Number == 1062
}

func init() {
	store.Register("mysql", &adapter{})
}