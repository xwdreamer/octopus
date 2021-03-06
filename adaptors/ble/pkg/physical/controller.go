package physical

import (
	"fmt"
	"strings"
	"time"

	"github.com/bettercap/gatt"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/octopus/adaptors/ble/api/v1alpha1"
)

type BLEController struct {
	done        chan struct{}
	endpoint    string
	properties  []v1alpha1.BluetoothDeviceProperty
	statusProps []v1alpha1.BluetoothDeviceStatusProperty
	log         logr.Logger
}

func (c *BLEController) onStateChanged(d gatt.Device, s gatt.State) {
	c.log.Info("Bluetooth state", s)
	switch s {
	case gatt.StatePoweredOn:
		c.log.Info("Scanning...")
		d.Scan([]gatt.UUID{}, false)
		return
	default:
		d.StopScanning()
	}
}

func (c *BLEController) onPeripheralDiscovered(p gatt.Peripheral, a *gatt.Advertisement, rssi int) {
	var endpoint = strings.ToUpper(c.endpoint)
	if strings.ToUpper(a.LocalName) != endpoint && strings.ToUpper(p.ID()) != endpoint {
		return
	}

	// Stop scanning once we've got the peripheral we're looking for.
	c.log.Info("Stop scanning and found device", a.LocalName)
	p.Device().StopScanning()
	c.log.V(2).Info("Peripheral ID, name", p.ID(), p.Name())
	p.Device().Connect(p)
}

func (c *BLEController) onPeripheralConnected(p gatt.Peripheral, err error) {
	c.log.Info("Connected to", p.Name())
	defer p.Device().CancelConnection(p)

	if err := p.SetMTU(500); err != nil {
		c.log.Error(err, "Failed to set MTU")
	}

	// Discovery services
	ss, err := p.DiscoverServices(nil)
	if err != nil {
		c.log.Error(err, "Failed to discover services")
		return
	}

	for _, svc := range ss {
		// Discovery characteristics
		cs, err := p.DiscoverCharacteristics(nil, svc)
		if err != nil {
			c.log.Error(err, "Failed to discover characteristics")
			continue
		}

		for _, ch := range cs {
			property, found := findCharacteristic(c.properties, svc.UUID().String())
			if !found {
				continue
			}

			switch property.AccessMode {
			case v1alpha1.BluetoothDevicePropertyReadOnly:
				{
					_, err := c.readCharacteristic(p, ch, property)
					if err != nil {
						c.log.Error(err, "Failed to read Characteristic")
						continue
					}
				}
			case v1alpha1.BluetoothDevicePropertyReadWrite:
				{
					err := c.writeCharacteristic(p, ch, property)
					if err != nil {
						c.log.Error(err, "Failed to write Characteristic")
						return
					}
				}
			case v1alpha1.BluetoothDevicePropertyNotifyOnly:
				{
					err := c.getNotifyCharacteristic(p, ch, property)
					if err != nil {
						c.log.Error(err, "Failed to get notify Characteristic")
						return
					}
				}
			default:
				c.log.Info("AccessMode is not defined or either not a valid option", property.AccessMode)
			}
		}
	}
	c.log.Info("Waiting for 5 seconds to get some notifications, if any.")
	time.Sleep(5 * time.Second)
}

func (c *BLEController) onPeriphDisconnected(p gatt.Peripheral, err error) {
	if c.done != nil {
		close(c.done)
	}
	p.Device().CancelConnection(p)
	c.log.Info("Device disconnected")
}

func findCharacteristic(properties []v1alpha1.BluetoothDeviceProperty, characteristicUUID string) (v1alpha1.BluetoothDeviceProperty, bool) {
	deviceProperty := v1alpha1.BluetoothDeviceProperty{}
	for _, p := range properties {
		if p.Visitor.CharacteristicUUID == characteristicUUID {
			return p, true
		}
	}
	return deviceProperty, false
}

func (c *BLEController) readCharacteristic(p gatt.Peripheral, ch *gatt.Characteristic, property v1alpha1.BluetoothDeviceProperty) (string, error) {
	b, err := p.ReadCharacteristic(ch)
	if err != nil {
		return "", err
	}
	c.log.Info("ReadCharacteristic value", string(b))

	convertedValue := fmt.Sprintf("%f", ConvertReadData(property.Visitor.DataConverter, b))
	c.log.Info("Converted read value to", convertedValue)
	c.updateDeviceStatus(property.Name, convertedValue, property.AccessMode)
	return convertedValue, nil
}

func (c *BLEController) writeCharacteristic(p gatt.Peripheral, ch *gatt.Characteristic, property v1alpha1.BluetoothDeviceProperty) error {
	if len(property.Visitor.DataWrite) == 0 {
		return fmt.Errorf("invalid length 0 of writeDataTo")
	}

	byteData, hasValue := findDataWriteToDeviceByDefaultValue(property.Visitor)
	if !hasValue {
		return fmt.Errorf("invalid length 0 of writeData")
	}

	err := p.WriteCharacteristic(ch, byteData, true)
	if err != nil {
		return err
	}

	value, err := c.readCharacteristic(p, ch, property)
	if err != nil {
		return err
	}
	c.updateDeviceStatus(property.Name, value, property.AccessMode)
	return nil
}

func (c *BLEController) getNotifyCharacteristic(p gatt.Peripheral, ch *gatt.Characteristic, property v1alpha1.BluetoothDeviceProperty) error {
	_, err := p.DiscoverDescriptors(nil, ch)
	if err != nil {
		return fmt.Errorf("failed to discover descriptors, %s", err.Error())
	}

	// Subscribe the characteristic, if possible.
	if (ch.Properties() & (gatt.CharNotify | gatt.CharIndicate)) != 0 {
		f := func(ch *gatt.Characteristic, b []byte, err error) {
			c.log.V(2).Info("Get notified data", string(b))
			c.updateDeviceStatus(property.Name, string(b), property.AccessMode)
		}
		if err := p.SetNotifyValue(ch, f); err != nil {
			return err
		}
	}
	return nil
}

func findDataWriteToDeviceByDefaultValue(visitor v1alpha1.BluetoothDevicePropertyVisitor) ([]byte, bool) {
	for k, v := range visitor.DataWrite {
		if visitor.DefaultValue == k {
			return v, true
		}
	}
	return nil, false
}

func (c *BLEController) updateDeviceStatus(name, value string, accessMode v1alpha1.BluetoothDevicePropertyAccessMode) {
	sp := v1alpha1.BluetoothDeviceStatusProperty{
		Name:       name,
		Value:      value,
		AccessMode: accessMode,
		UpdatedAt:  now(),
	}
	found := false
	for i, property := range c.statusProps {
		if property.Name == sp.Name {
			c.statusProps[i] = sp
			found = true
			break
		}
	}
	if !found {
		c.statusProps = append(c.statusProps, sp)
	}
}

func now() *metav1.Time {
	var ret = metav1.Now()
	return &ret
}
