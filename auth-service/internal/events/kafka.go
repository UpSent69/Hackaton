// Package events — обмен сообщениями между микросервисами через Kafka.
//
//	documents.parse    core → ai-service            файл поставки или заявки на распознавание
//	documents.results  ai-service → core            результат распознавания (ответ для GET /api/jobs/{id})
//	analytics.requests core → ai-service            статистика для анализа Qwen
//	analytics.results  ai-service → core            готовый анализ или ошибка
//	warehouse.events   core → notification-service  события склада, из которых строятся уведомления
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	TopicDocumentJobs      = "documents.parse"
	TopicDocumentResults   = "documents.results"
	TopicAnalyticsRequests = "analytics.requests"
	TopicAnalyticsResults  = "analytics.results"
	TopicWarehouseEvents   = "warehouse.events"
)

// Фото накладной до 12 МБ уходит в сообщении целиком (base64 в JSON), поэтому лимит
// сообщения поднят до 32 МБ — так же он настроен у брокера и у топиков.
const maxMessageBytes = 32 << 20

// ErrDisabled — Kafka не настроена (KAFKA_BROKERS пустой).
var ErrDisabled = errors.New("kafka не настроена")

// Bus — подключение к Kafka. Нулевой *Bus означает «Kafka не настроена».
type Bus struct {
	brokers []string
	sync    *kafka.Writer
	async   *kafka.Writer
}

// New подключается к брокерам из списка «host:port,host:port». Пустая строка — Kafka выключена.
func New(brokers string) *Bus {
	list := make([]string, 0)
	for _, broker := range strings.Split(brokers, ",") {
		if broker = strings.TrimSpace(broker); broker != "" {
			list = append(list, broker)
		}
	}
	if len(list) == 0 {
		log.Println("Kafka: KAFKA_BROKERS не задан — обмен событиями выключен")
		return nil
	}
	writer := func(async bool) *kafka.Writer {
		return &kafka.Writer{
			Addr:                   kafka.TCP(list...),
			Balancer:               &kafka.Hash{},
			BatchTimeout:           10 * time.Millisecond,
			BatchBytes:             maxMessageBytes,
			RequiredAcks:           kafka.RequireOne,
			AllowAutoTopicCreation: true,
			Async:                  async,
			Completion: func(messages []kafka.Message, err error) {
				if err != nil && async {
					log.Printf("Kafka: не удалось отправить %d сообщ. в %s: %v", len(messages), messages[0].Topic, err)
				}
			},
		}
	}
	bus := &Bus{brokers: list, sync: writer(false), async: writer(true)}
	go bus.ensureTopics()
	return bus
}

// Enabled — настроена ли Kafka.
func (b *Bus) Enabled() bool { return b != nil }

// Publish отправляет сообщение и ждёт подтверждения брокера.
func (b *Bus) Publish(ctx context.Context, topic, key string, value any) error {
	if b == nil {
		return ErrDisabled
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.sync.WriteMessages(ctx, kafka.Message{Topic: topic, Key: []byte(key), Value: encoded})
}

// PublishAsync отправляет сообщение, не дожидаясь брокера (ошибки пишутся в лог).
func (b *Bus) PublishAsync(topic, key string, value any) {
	if b == nil {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		log.Printf("Kafka: не удалось закодировать сообщение для %s: %v", topic, err)
		return
	}
	_ = b.async.WriteMessages(context.Background(), kafka.Message{Topic: topic, Key: []byte(key), Value: encoded})
}

// Consume в фоне читает топик в группе потребителей group и передаёт каждое сообщение
// в handle. Смещение фиксируется после обработки: при перезапуске сервиса непрочитанное
// не теряется. Пока брокер недоступен, чтение повторяется.
func (b *Bus) Consume(topic, group string, handle func(value []byte)) {
	if b == nil {
		return
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        b.brokers,
		GroupID:        group,
		Topic:          topic,
		MaxBytes:       maxMessageBytes,
		StartOffset:    kafka.FirstOffset,
		MaxWait:        500 * time.Millisecond,
		CommitInterval: 0,
	})
	go func() {
		log.Printf("Kafka: %s читает топик %s", group, topic)
		for {
			message, err := reader.FetchMessage(context.Background())
			if err != nil {
				log.Printf("Kafka: чтение %s: %v — повтор через 3 с", topic, err)
				time.Sleep(3 * time.Second)
				continue
			}
			handle(message.Value)
			if err := reader.CommitMessages(context.Background(), message); err != nil {
				log.Printf("Kafka: не удалось зафиксировать смещение %s: %v", topic, err)
			}
		}
	}()
}

// ensureTopics создаёт топики с поднятым лимитом сообщения; повторяет, пока брокер не поднимется.
func (b *Bus) ensureTopics() {
	topics := []string{TopicDocumentJobs, TopicDocumentResults, TopicAnalyticsRequests, TopicAnalyticsResults, TopicWarehouseEvents}
	for {
		err := b.createTopics(topics)
		if err == nil {
			log.Printf("Kafka: подключена (%s), топики готовы", strings.Join(b.brokers, ","))
			return
		}
		log.Printf("Kafka: брокер пока недоступен (%v) — повтор через 3 с", err)
		time.Sleep(3 * time.Second)
	}
}

func (b *Bus) createTopics(topics []string) error {
	conn, err := kafka.Dial("tcp", b.brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	controllerConn, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer controllerConn.Close()
	configs := make([]kafka.TopicConfig, 0, len(topics))
	for _, topic := range topics {
		configs = append(configs, kafka.TopicConfig{
			Topic: topic, NumPartitions: 1, ReplicationFactor: 1,
			ConfigEntries: []kafka.ConfigEntry{{ConfigName: "max.message.bytes", ConfigValue: strconv.Itoa(maxMessageBytes)}},
		})
	}
	return controllerConn.CreateTopics(configs...)
}
