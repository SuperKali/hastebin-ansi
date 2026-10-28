package storage

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type MongoDBStorage struct {
	db         *mongo.Database
	collection *mongo.Collection
	expiration time.Duration
}

type item struct {
	ObjectID   any       `json:"_id,omitempty" bson:"_id,omitempty"`
	Key        string    `json:"key" bson:"key"`
	Value      []byte    `json:"value" bson:"value"`
	Size       int64     `json:"size" bson:"size"`
	Expiration time.Time `json:"expiration,omitempty" bson:"expiration,omitempty"`
}

func NewMongoDBStorage(host string, port int, username string, password string, database string, expiration time.Duration) *MongoDBStorage {
	ctx := context.Background()

	dsn := "mongodb://"
	if username != "" {
		dsn += url.QueryEscape(username)
	}

	if password != "" {
		dsn += ":" + url.QueryEscape(password)
	}

	if username != "" || password != "" {
		dsn += "@"
	}

	dsn += host + ":" + strconv.Itoa(port)

	client, err := mongo.Connect(options.Client().ApplyURI(dsn))
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to MongoDB")
	}

	// Check if connection is established
	if err := client.Ping(ctx, nil); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to MongoDB")
	}

	db := client.Database(database)

	// Create the collection; on a restart it already exists
	command := bson.M{"create": "entries"}
	var result bson.M
	if err := db.RunCommand(ctx, command).Decode(&result); err != nil && !isNamespaceExists(err) {
		log.Fatal().Err(err).Str("database", database).Msg("Failed to create the MongoDB collection entries")
	}

	// Create collection if not exists
	collection := db.Collection("entries")

	indexModel := mongo.IndexModel{
		Keys:    bson.M{"expiration": 1},
		Options: options.Index().SetExpireAfterSeconds(0),
	}

	if _, err := collection.Indexes().CreateOne(ctx, indexModel); err != nil {
		log.Fatal().Err(err).Str("database", database).Msg("Failed to create the MongoDB expiration index")
	}

	keyIndexModel := mongo.IndexModel{
		Keys:    bson.M{"key": 1},
		Options: options.Index().SetUnique(true),
	}

	if _, err := collection.Indexes().CreateOne(ctx, keyIndexModel); err != nil {
		log.Fatal().Err(err).Str("database", database).Msg("Failed to create the MongoDB key index")
	}

	return &MongoDBStorage{db: db, collection: collection, expiration: expiration}
}

// namespaceExists is the MongoDB error code for creating a collection that exists
const namespaceExists = 48

// isNamespaceExists reports whether a MongoDB error means the collection already exists
func isNamespaceExists(err error) bool {
	var serverErr mongo.ServerError
	return errors.As(err, &serverErr) && serverErr.HasErrorCode(namespaceExists)
}

func (s *MongoDBStorage) Set(key string, value string, skip_expiration bool) error {
	ctx := context.Background()

	// Convert value to []byte
	valueBytes := []byte(value)

	expiration := time.Now().Add(s.expiration)
	if skip_expiration || s.expiration <= 0 {
		expiration = time.Time{}
	}

	// Create item
	i := item{
		Key:        key,
		Value:      valueBytes,
		Size:       int64(len(valueBytes)),
		Expiration: expiration,
	}

	// Insert or replace item, so static documents can be set again on restart
	filter := bson.M{"key": key}
	if _, err := s.collection.ReplaceOne(ctx, filter, i, options.Replace().SetUpsert(true)); err != nil {
		return err
	}

	return nil
}

func (s *MongoDBStorage) Get(key string, skip_expiration bool) (string, error) {
	ctx := context.Background()

	// Find item
	filter := bson.M{"key": key}
	var i item
	if err := s.collection.FindOne(ctx, filter).Decode(&i); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", notFound(err)
		}
		return "", err
	}

	if !i.Expiration.IsZero() && i.Expiration.Unix() <= time.Now().Unix() {
		_, err := s.collection.DeleteOne(ctx, filter)
		if err != nil {
			return "", err
		}

		return "", ErrNotFound
	}

	// Update expiration, only for entries that expire
	if !skip_expiration && !i.Expiration.IsZero() && s.expiration > 0 {
		i.Expiration = time.Now().Add(s.expiration)
		update := bson.M{"$set": bson.M{"expiration": i.Expiration}}
		if _, err := s.collection.UpdateOne(ctx, filter, update); err != nil {
			return "", err
		}

	}

	return string(i.Value), nil
}

func (s *MongoDBStorage) Size(key string) (int64, error) {
	ctx := context.Background()

	// Entries written before the size field existed are measured on the server
	projection := bson.M{
		"_id":        0,
		"expiration": 1,
		"size":       bson.M{"$ifNull": bson.A{"$size", bson.M{"$binarySize": "$value"}}},
	}
	var res struct {
		Size       int64     `bson:"size"`
		Expiration time.Time `bson:"expiration,omitempty"`
	}
	err := s.collection.FindOne(ctx, bson.M{"key": key}, options.FindOne().SetProjection(projection)).Decode(&res)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, notFound(err)
	}
	if err != nil {
		return 0, err
	}

	// Expired entries are deleted by the next Get or the TTL index
	if !res.Expiration.IsZero() && res.Expiration.Unix() <= time.Now().Unix() {
		return 0, ErrNotFound
	}
	return res.Size, nil
}

func (s *MongoDBStorage) Close() error {
	return s.db.Client().Disconnect(context.Background())
}
