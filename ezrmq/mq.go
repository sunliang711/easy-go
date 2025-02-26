package ezrmq

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitMQ struct {
	config       Config
	conn         *amqp.Connection
	ch           *amqp.Channel
	wg           sync.WaitGroup
	reconnectMux sync.Mutex
	consumeMux   sync.Mutex

	logger *log.Logger

	ctx        context.Context
	cancelFunc context.CancelFunc
}

// NewRabbitMQ creates a new RabbitMQ instance
// caCertBytes如果为空，则不使用服务端TLS
// clientCert和clientKey如果为空，则不使用客户端TLS
func NewRabbitMQ(url string, reconnect int, caCertBytes, clientCert, clientKey []byte) (*RabbitMQ, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &RabbitMQ{
		config: Config{
			URL:          url,
			ReconnectSec: reconnect,
			CaCertBytes:  caCertBytes,
			ClientCert:   clientCert,
			ClientKey:    clientKey,
			Consumers:    make(map[string][]ConsumerConfig),
			Producers:    make(map[string]ProducerConfig),
		},
		// closeCh:    make(chan struct{}),
		logger:     log.New(os.Stdout, "|RMQ| ", log.LstdFlags),
		ctx:        ctx,
		cancelFunc: cancel,
	}

	return r, nil
}

func (r *RabbitMQ) AddConsumer(exchangeName string, topic string, handler MessageHandlerFunc, queueOptions QueueOptions, consumeOptions ConsumeOptions) error {
	config := ConsumerConfig{
		// ExchangeOptions: exchangeOptions,
		Handler:        handler,
		Topic:          topic,
		QueueOptions:   queueOptions,
		ConsumeOptions: consumeOptions,
	}

	if handler == nil {
		return fmt.Errorf("exchange %s handler is nil", exchangeName)
	}

	r.config.Consumers[exchangeName] = append(r.config.Consumers[exchangeName], config)
	return nil
}

func (r *RabbitMQ) AddProducer(exchangeName string, exchangeOptions ExchangeOptions) error {
	exchange := ProducerConfig{
		ExchangeOptions: exchangeOptions,
	}

	r.config.Producers[exchangeName] = exchange
	return nil
}

func (r *RabbitMQ) Connect() error {
	r.reconnectMux.Lock()
	defer r.reconnectMux.Unlock()

	var err error

	// cancelFunc 非空，使用服务端TLS
	if len(r.config.CaCertBytes) > 0 {
		r.logger.Printf("config server ca certificate")
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(r.config.CaCertBytes)

		var clientCertificates []tls.Certificate

		// 如果clientCert和clientKey非空，则使用客户端TLS
		if len(r.config.ClientCert) > 0 && len(r.config.ClientKey) > 0 {
			r.logger.Printf("config client certificate")
			clientCert, err := tls.X509KeyPair(r.config.ClientCert, r.config.ClientKey)
			if err != nil {
				return err
			}
			clientCertificates = append(clientCertificates, clientCert)
		}

		tlsConfig := &tls.Config{
			RootCAs:      caCertPool,
			Certificates: clientCertificates,
		}
		r.conn, err = amqp.DialTLS(r.config.URL, tlsConfig)
		if err != nil {
			return err
		}
	} else {
		r.conn, err = amqp.Dial(r.config.URL)
		if err != nil {
			return err
		}
	}

	// 创建channel
	if err := r.setupChannel(); err != nil {
		r.conn.Close()
		return err
	}

	r.handleReconnect()

	// 声明produce exchange
	for exchangeName, producer := range r.config.Producers {
		r.logger.Printf("Declare exchange: %s", exchangeName)
		err = r.ch.ExchangeDeclare(
			exchangeName,                       // name
			producer.ExchangeOptions.Type,      // type
			producer.ExchangeOptions.Durable,   // durable
			producer.ExchangeOptions.AutoDel,   // auto-deleted
			producer.ExchangeOptions.Internal,  // internal
			producer.ExchangeOptions.NoWait,    // no-wait
			producer.ExchangeOptions.Arguments, // arguments
		)
		if err != nil {
			return err
		}

	}

	// 遍历consume exchange, 声明queue并绑定exchange
	for exchangeName, consumers := range r.config.Consumers {
		for _, consumer := range consumers {
			ch, err := r.consume(exchangeName, &consumer)
			if err != nil {
				return err
			}
			go messageHandler(r.ctx, ch, consumer.Handler)
		}
	}

	return nil
}

// 新增setupChannel方法
func (r *RabbitMQ) setupChannel() error {
	var err error
	r.ch, err = r.conn.Channel()
	if err != nil {
		return err
	}

	// // 监听channel关闭
	// go func() {
	// 	// 注意这里要先获取channel的引用，因为重连时r.ch会变化
	// 	ch := r.ch
	// 	chClose := ch.NotifyClose(make(chan *amqp.Error))
	// 	select {
	// 	case err := <-chClose:
	// 		r.logger.Printf("Channel closed: %v", err)
	// 		// 重新设置channel
	// 		r.reconnectChannel()
	// 	case <-r.ctx.Done():
	// 		return
	// 	}
	// }()

	return nil
}

// // 新增channel重连方法
// func (r *RabbitMQ) reconnectChannel() {
// 	r.reconnectMux.Lock()
// 	defer r.reconnectMux.Unlock()

// 	for {
// 		r.logger.Println("Attempting to reconnect channel...")

// 		// 检查connection是否正常，如果connection断开，会触发connection的重连
// 		if r.conn.IsClosed() {
// 			r.logger.Println("Connection is closed, waiting for connection recovery...")
// 			time.Sleep(time.Duration(r.config.ReconnectSec) * time.Second)
// 			continue
// 		}

// 		// 重新设置channel
// 		if err := r.setupChannel(); err != nil {
// 			r.logger.Printf("Failed to recreate channel: %s. Retrying in %d seconds...", err, r.config.ReconnectSec)
// 			time.Sleep(time.Duration(r.config.ReconnectSec) * time.Second)
// 			continue
// 		}

// 		// 重新声明exchanges
// 		for exchangeName, producer := range r.config.Producers {
// 			err := r.ch.ExchangeDeclare(
// 				exchangeName,
// 				producer.ExchangeOptions.Type,
// 				producer.ExchangeOptions.Durable,
// 				producer.ExchangeOptions.AutoDel,
// 				producer.ExchangeOptions.Internal,
// 				producer.ExchangeOptions.NoWait,
// 				producer.ExchangeOptions.Arguments,
// 			)
// 			if err != nil {
// 				r.logger.Printf("Failed to declare exchange: %s. Retrying...", err)
// 				time.Sleep(time.Duration(r.config.ReconnectSec) * time.Second)
// 				continue
// 			}
// 		}

// 		// 重新设置consumers
// 		for exchangeName, consumers := range r.config.Consumers {
// 			for _, consumer := range consumers {
// 				ch, err := r.consume(exchangeName, &consumer)
// 				if err != nil {
// 					r.logger.Printf("Failed to recreate consumer: %s. Retrying...", err)
// 					time.Sleep(time.Duration(r.config.ReconnectSec) * time.Second)
// 					continue
// 				}
// 				go messageHandler(r.ctx, ch, consumer.Handler)
// 			}
// 		}

// 		r.logger.Println("Channel successfully reconnected")
// 		return
// 	}
// }

func (r *RabbitMQ) reconnect() {
	for {
		r.logger.Println("Attempting to reconnect...")
		err := r.Connect()
		if err == nil {
			r.logger.Println("Reconnected to RabbitMQ")
			return
		}
		r.logger.Printf("Failed to reconnect: %s. Retrying in %d seconds...", err, r.config.ReconnectSec)
		time.Sleep(time.Duration(r.config.ReconnectSec) * time.Second)
	}
}

func (r *RabbitMQ) handleReconnect() {
	// go func() {
	// 	err := <-r.conn.NotifyClose(make(chan *amqp.Error))
	// 	if err != nil {
	// 		r.logger.Printf("Connection closed: %s", err)
	// 		r.reconnect()
	// 	}
	// }()
	r.check()
}

func (r *RabbitMQ) check() {
	go func() {
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(time.Second * 5):
				if r.conn.IsClosed() {
					r.reconnect()
				}
			}
		}
	}()
}

// TODO: mandatory immediate
func (r *RabbitMQ) Publish(exchange, routingKey string, body []byte) error {
	return r.ch.Publish(
		exchange,   // exchange
		routingKey, // routing key
		false,      // mandatory
		false,      // immediate
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        body,
		},
	)
}

func (r *RabbitMQ) consume(exchangeName string, consumerConfig *ConsumerConfig) (<-chan amqp.Delivery, error) {
	r.consumeMux.Lock()
	defer r.consumeMux.Unlock()

	var queue amqp.Queue
	var err error
	maxRetries := 5
	retryInterval := time.Second * 2

	for i := 0; i < maxRetries; i++ {
		r.logger.Printf("Declare queue: %s", consumerConfig.QueueOptions.Name)
		queue, err = r.ch.QueueDeclare(
			consumerConfig.QueueOptions.Name,      // name
			consumerConfig.QueueOptions.Durable,   // durable
			consumerConfig.QueueOptions.AutoDel,   // delete when unused
			consumerConfig.QueueOptions.Exclusive, // exclusive
			consumerConfig.QueueOptions.NoWait,    // no-wait
			consumerConfig.QueueOptions.Arguments, // arguments
		)
		if err == nil {
			break
		}
		r.logger.Printf("Declare queue error: %v. Retrying in %v...", err, retryInterval)
		time.Sleep(retryInterval)
	}

	if err != nil {
		return nil, fmt.Errorf("declare queue: %s error: %w", consumerConfig.QueueOptions.Name, err)
	}

	// 绑定exchange和queue
	for i := 0; i < maxRetries; i++ {
		r.logger.Printf("Bind queue: %v to exchange: %v with topic: %v", queue.Name, exchangeName, consumerConfig.Topic)
		err = r.ch.QueueBind(
			queue.Name,           // queue name
			consumerConfig.Topic, // routing key
			exchangeName,         // exchange
			false,
			nil,
		)
		if err == nil {
			break
		}
		r.logger.Printf("QueueBind error: %v. Retrying in %v...", err, retryInterval)
		time.Sleep(retryInterval)
	}

	if err != nil {
		return nil, fmt.Errorf("QueueBind error: %w", err)
	}

	r.logger.Printf("Consume with queue: %s", queue.Name)
	msgs, err := r.ch.Consume(
		queue.Name,                              // queue
		"",                                      // consumer
		consumerConfig.ConsumeOptions.AutoAck,   // auto-ack
		consumerConfig.ConsumeOptions.Exclusive, // exclusive
		consumerConfig.ConsumeOptions.NoLocal,   // no-local
		consumerConfig.ConsumeOptions.NoWait,    // no-wait
		consumerConfig.ConsumeOptions.Arguments, // args
	)
	if err != nil {
		return nil, fmt.Errorf("Consume error: %w", err)
	}

	out := make(chan amqp.Delivery)
	r.wg.Add(1)
	go func() {
		defer func() {
			defer r.wg.Done()
			defer close(out)
			r.logger.Printf("Consume from queue: %s exit", queue.Name)
		}()

		r.logger.Printf("Receive messages from queue: %s", queue.Name)
		for {
			select {
			case msg, ok := <-msgs:
				if !ok {
					r.logger.Printf("Channel closed,exit")
					return
				}
				out <- msg
			case <-r.ctx.Done():
				r.logger.Printf("Context done,exit")
				return
			}
		}
	}()
	return out, nil
}

func (r *RabbitMQ) Close() {
	r.logger.Println("Closing RabbitMQ connection...")
	r.cancelFunc()
	r.logger.Println("Waiting for all consumers to stop...")
	r.wg.Wait()
	r.logger.Println("All consumers stopped")
	if err := r.ch.Close(); err != nil {
		r.logger.Printf("Failed to close channel: %s", err)
	}
	if err := r.conn.Close(); err != nil {
		r.logger.Printf("Failed to close connection: %s", err)
	}
}
