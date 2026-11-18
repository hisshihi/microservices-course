package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	googleuuid "github.com/google/uuid"
	orderv1 "github.com/hisshihi/microservices-course/shared/pkg/openapi/order/v1"
)

var ErrOrderNotFound = errors.New("order with uuid not found")

const (
	httpAddr          = "8080"
	readHeaderTimeout = 5 * time.Second
	serverTimeout     = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

type Status string

const (
	PendingPayment Status = "PENDING_PAYMENT"
	Paid           Status = "PAID"
	Cancelled      Status = "CANCELLED"
)

func (s Status) String() string {
	return string(s)
}

type PaymentMethod string

const (
	Unknown       PaymentMethod = "UNKNOWN"
	Card          PaymentMethod = "CARD"
	SBP           PaymentMethod = "SBP"
	CreditCard    PaymentMethod = "CREDIT_CARD"
	InvestorMoney PaymentMethod = "INVESTOR_MONEY"
)

func (s PaymentMethod) String() string {
	return string(s)
}

type Order struct {
	UserUUID        uuid.UUID      `json:"user_uuid"`
	OrderUUID       uuid.UUID      `json:"order_uuid"`
	PartUUIDs       []uuid.UUID    `json:"part_uuids"`
	TotalPrice      float64        `json:"total_price"`
	TransactionUUID *uuid.UUID     `json:"transaction_uuid"` // Только если оплачено
	PaymentMethod   *PaymentMethod `json:"payment_method"`
	Status          Status         `json:"status"`
}

type OrderStorage struct {
	mu    sync.Mutex
	order map[string]*Order
}

func NewOrderStorage() *OrderStorage {
	return &OrderStorage{
		order: make(map[string]*Order),
	}
}

func (s *OrderStorage) Save(userUUID uuid.UUID, partUUIDs []uuid.UUID) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.order[userUUID.String()]
	if ok {
		order.PartUUIDs = append(order.PartUUIDs, partUUIDs...)
		order.Status = PendingPayment
		return order, nil
	}
	order = &Order{
		UserUUID:  userUUID,
		OrderUUID: uuid.NewV7(),
		PartUUIDs: partUUIDs,
		Status:    PendingPayment,
	}
	s.order[order.OrderUUID.String()] = order
	return order, nil
}

func (s *OrderStorage) GetOrder(orderUUID string) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.order[orderUUID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	return order, nil
}

func (s *OrderStorage) mapOrderStorageToOrderv1(orderUUIDStr string) (*orderv1.Order, error) {
	order, err := s.GetOrder(orderUUIDStr)
	if err != nil {
		return nil, err
	}

	googleUUIDs := make([]googleuuid.UUID, 0, len(order.PartUUIDs))
	for _, partUUID := range order.PartUUIDs {
		googleUUIDs = append(googleUUIDs, googleuuid.UUID(partUUID))
	}

	var transactionUUID orderv1.OptUUID
	if order.TransactionUUID != nil {
		transactionUUID = orderv1.OptUUID{Set: true, Value: googleuuid.UUID(*order.TransactionUUID)}
	} else {
		transactionUUID = orderv1.OptUUID{Set: false}
	}

	var paymentMethod orderv1.OptPaymentMethodEnum
	if order.PaymentMethod != nil {
		paymentMethodString := order.PaymentMethod.String()
		paymentMethod = orderv1.OptPaymentMethodEnum{Set: true, Value: orderv1.PaymentMethodEnum(paymentMethodString)}
	} else {
		paymentMethod = orderv1.OptPaymentMethodEnum{Set: false}
	}

	return &orderv1.Order{
		UserUUID:        googleuuid.UUID(order.UserUUID),
		OrderUUID:       googleuuid.UUID(order.OrderUUID),
		PartUuids:       googleUUIDs,
		TotalPrice:      float32(order.TotalPrice),
		TransactionUUID: transactionUUID,
		PaymentMethod:   paymentMethod,
		Status:          orderv1.StatusEnum(order.Status.String()),
	}, nil
}

type OrderHandler struct {
	orderStorage *OrderStorage
}

func NewOrderHandler(orderStorage *OrderStorage) *OrderHandler {
	return &OrderHandler{orderStorage: orderStorage}
}

func (h *OrderHandler) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (orderv1.CreateOrderRes, error) {
	parts := make([]uuid.UUID, 0, len(req.GetPartUuids()))
	for _, part := range req.GetPartUuids() {
		parts = append(parts, uuid.UUID(part))
	}
	save, err := h.orderStorage.Save(uuid.UUID(req.UserUUID), parts)
	if err != nil {
		log.Printf("Ошибка при сохранении заказа: %v\n", err)
		return nil, h.NewError(ctx, err)
	}

	return &orderv1.CreateOrderResponse{OrderUUID: save.OrderUUID.String(), TotalPrice: float32(save.TotalPrice)}, nil
}

func (h *OrderHandler) GetOrderByUUID(ctx context.Context, params orderv1.GetOrderByUUIDParams) (orderv1.GetOrderByUUIDRes, error) {
	order, err := h.orderStorage.mapOrderStorageToOrderv1(params.OrderUUID.String())
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) {
			return &orderv1.GetOrderNotFound{
				Code:    "order_not_found",
				Message: "Заказ не найден",
			}, nil
		}
		return nil, h.NewError(ctx, err)
	}

	return order, nil
}

func (h *OrderHandler) NewError(_ context.Context, err error) *orderv1.GenericErrorStatusCode {
	return &orderv1.GenericErrorStatusCode{
		StatusCode: http.StatusInternalServerError,
		Response: orderv1.GenericError{
			Code:    orderv1.NewOptString("internal_error"),
			Message: orderv1.NewOptString(fmt.Sprintf("Ошибка при выполнении запроса: %v", err)),
		},
	}
}

func main() {
	storage := NewOrderStorage()

	handler := NewOrderHandler(storage)

	orderServer, err := orderv1.NewServer(handler)
	if err != nil {
		log.Fatalf("ошибка при создании сервера openAPI: %v", err)
	}

	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(serverTimeout))

	r.Mount("/", orderServer)

	server := &http.Server{
		Addr:              net.JoinHostPort("localhost", httpAddr),
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		log.Printf("запуск сервера на %s", server.Addr)
		if srvErr := server.ListenAndServe(); srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
			log.Printf("ошибка запуска сервера: %v", srvErr)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Println("Завершение работы")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		log.Printf("ошибка при завершении работы сервера: %v", err)
	}
	log.Println("Сервер успешно остановлен")
}
