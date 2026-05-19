package usecase

import (
	"context"
	"fmt"

	domainDevice "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/device"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/whatsapp"
	pkgError "github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/error"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/websocket"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/validations"
	"github.com/sirupsen/logrus"
	"go.mau.fi/whatsmeow"
)

type serviceDevice struct {
	manager *whatsapp.DeviceManager
}

func NewDeviceService(manager *whatsapp.DeviceManager) domainDevice.IDeviceUsecase {
	return &serviceDevice{
		manager: manager,
	}
}

func (s *serviceDevice) ListDevices(_ context.Context) ([]domainDevice.Device, error) {
	if s.manager == nil {
		return []domainDevice.Device{}, nil
	}

	var result []domainDevice.Device
	for _, inst := range s.manager.ListDevices() {
		inst.UpdateStateFromClient()
		result = append(result, convertInstance(inst))
	}
	return result, nil
}

func (s *serviceDevice) GetDevice(_ context.Context, deviceID string) (*domainDevice.Device, error) {
	if s.manager == nil {
		return nil, fmt.Errorf("device manager not initialized")
	}
	if inst, ok := s.manager.GetDevice(deviceID); ok {
		device := convertInstance(inst)
		return &device, nil
	}
	return nil, fmt.Errorf("device %s not found", deviceID)
}

func (s *serviceDevice) AddDevice(ctx context.Context, deviceID string) (*domainDevice.Device, error) {
	if s.manager == nil {
		return nil, fmt.Errorf("device manager not initialized")
	}

	inst, err := s.manager.CreateDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	device := convertInstance(inst)
	return &device, nil
}

func (s *serviceDevice) RemoveDevice(_ context.Context, deviceID string) error {
	if s.manager == nil {
		return fmt.Errorf("device manager not initialized")
	}
	s.manager.RemoveDevice(deviceID)
	return nil
}

func (s *serviceDevice) LoginDevice(_ context.Context, _ string) error {
	return fmt.Errorf("device login per ID is not implemented yet")
}

// LoginDeviceWithCode generates an 8-character WhatsApp pairing code for the
// given device, so the coach can finish the link from WhatsApp Business
// -> Settings -> Linked devices -> "Link with phone number" without ever
// scanning a QR. Mirrors serviceApp.LoginWithCode (single-device path that
// has been working since v6.x) but scoped per device_id via the manager,
// which is the missing piece for our multi-coach (multi-device) setup.
func (s *serviceDevice) LoginDeviceWithCode(ctx context.Context, deviceID string, phoneNumber string) (string, error) {
	if s.manager == nil {
		return "", fmt.Errorf("device manager not initialized")
	}
	if err := validations.ValidateLoginWithCode(ctx, phoneNumber); err != nil {
		logrus.Errorf("[LOGIN_CODE][%s] phone validation failed: %s", deviceID, err.Error())
		return "", err
	}

	instance, err := s.manager.EnsureClient(ctx, deviceID)
	if err != nil {
		return "", err
	}
	client := instance.GetClient()
	if client == nil {
		return "", pkgError.ErrWaCLI
	}

	if client.IsLoggedIn() {
		instance.UpdateStateFromClient()
		return "", pkgError.ErrAlreadyLoggedIn
	}

	// PairPhone requires an active socket; reconnect if the device is in
	// an idle state (e.g. just brought up from disk via EnsureClient).
	if !client.IsConnected() {
		if err = client.Connect(); err != nil {
			return "", err
		}
	}

	logrus.Infof("[LOGIN_CODE][%s] requesting pair code for %s", deviceID, phoneNumber)
	loginCode, err := client.PairPhone(ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		logrus.Errorf("[LOGIN_CODE][%s] PairPhone failed: %s", deviceID, err.Error())
		return "", err
	}

	instance.UpdateStateFromClient()
	logrus.Infof("[LOGIN_CODE][%s] pair code issued: %s", deviceID, loginCode)
	return loginCode, nil
}

func (s *serviceDevice) LogoutDevice(ctx context.Context, deviceID string) error {
	if s.manager == nil {
		return fmt.Errorf("device manager not initialized")
	}

	if err := s.manager.PurgeDevice(ctx, deviceID); err != nil {
		return err
	}

	// Broadcast device removal so UI clients can refresh.
	var devices []domainDevice.Device
	if s.manager != nil {
		for _, inst := range s.manager.ListDevices() {
			inst.UpdateStateFromClient()
			devices = append(devices, convertInstance(inst))
		}
	}

	websocket.Broadcast <- websocket.BroadcastMessage{
		Code:    "DEVICE_REMOVED",
		Message: fmt.Sprintf("Device %s logged out and removed", deviceID),
		Result: map[string]any{
			"device_id": deviceID,
			"devices":   devices,
		},
	}

	return nil
}

func (s *serviceDevice) ReconnectDevice(_ context.Context, deviceID string) error {
	if s.manager == nil {
		return fmt.Errorf("device manager not initialized")
	}
	if inst, ok := s.manager.GetDevice(deviceID); ok {
		client := inst.GetClient()
		if client == nil {
			return fmt.Errorf("device %s client not initialized", deviceID)
		}

		if client.Store == nil || client.Store.ID == nil {
			return fmt.Errorf("device %s is not logged in (session deleted)", deviceID)
		}

		client.Disconnect()
		return client.Connect()
	}
	return fmt.Errorf("device %s not found", deviceID)
}

func (s *serviceDevice) GetStatus(_ context.Context, deviceID string) (bool, bool, error) {
	if s.manager == nil {
		return false, false, fmt.Errorf("device manager not initialized")
	}
	if inst, ok := s.manager.GetDevice(deviceID); ok {
		inst.UpdateStateFromClient()
		client := inst.GetClient()
		if client == nil {
			return false, false, nil
		}

		if client.Store == nil || client.Store.ID == nil {
			return false, false, nil
		}

		// Update state snapshot based on live client flags
		state := deriveState(inst)
		_ = state
		return client.IsConnected(), client.IsLoggedIn(), nil
	}
	return false, false, fmt.Errorf("device %s not found", deviceID)
}

func convertInstance(inst *whatsapp.DeviceInstance) domainDevice.Device {
	if inst == nil {
		return domainDevice.Device{}
	}

	state := deriveState(inst)

	return domainDevice.Device{
		ID:          inst.ID(),
		PhoneNumber: inst.PhoneNumber(),
		DisplayName: inst.DisplayName(),
		State:       state,
		JID:         inst.JID(),
		CreatedAt:   inst.CreatedAt(),
	}
}

func deriveState(inst *whatsapp.DeviceInstance) domainDevice.DeviceState {
	if inst == nil {
		return domainDevice.DeviceStateDisconnected
	}

	client := inst.GetClient()
	state := inst.State()
	if client != nil {
		if client.IsLoggedIn() {
			state = domainDevice.DeviceStateLoggedIn
		} else if client.IsConnected() {
			state = domainDevice.DeviceStateConnected
		} else {
			state = domainDevice.DeviceStateDisconnected
		}
		inst.SetState(state)
	}

	return state
}
