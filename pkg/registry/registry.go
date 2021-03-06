package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd"
	log "github.com/golang/glog"
	"github.com/openshift/openshift-sdn/pkg/api"
)

type EtcdConfig struct {
	Endpoints        []string
	Keyfile          string
	Certfile         string
	CAFile           string
	SubnetPath       string
	SubnetConfigPath string
	MinionPath       string
}

type EtcdSubnetRegistry struct {
	mux     sync.Mutex
	cli     *etcd.Client
	etcdCfg *EtcdConfig
}

func newMinionEvent(action, key, value string) *api.MinionEvent {
	min := &api.MinionEvent{}
	switch action {
	case "delete", "deleted", "expired":
		min.Type = api.Deleted
	default:
		min.Type = api.Added
	}

	if key != "" {
		_, min.Minion = path.Split(key)
		return min
	}

	fmt.Printf("Error decoding minion event: nil key (%s,%s,%s).\n", action, key, value)
	return nil
}

func newSubnetEvent(resp *etcd.Response) *api.SubnetEvent {
	var value string
	_, minkey := path.Split(resp.Node.Key)
	var t api.EventType
	switch resp.Action {
	case "deleted", "delete", "expired":
		t = api.Deleted
		value = resp.PrevNode.Value
	default:
		t = api.Added
		value = resp.Node.Value
	}
	var sub api.Subnet
	if err := json.Unmarshal([]byte(value), &sub); err == nil {
		return &api.SubnetEvent{
			Type:   t,
			Minion: minkey,
			Sub:    sub,
		}
	}
	log.Errorf("Failed to unmarshal response: %v", resp)
	return nil
}

func newEtcdClient(c *EtcdConfig) (*etcd.Client, error) {
	if c.Keyfile != "" || c.Certfile != "" || c.CAFile != "" {
		return etcd.NewTLSClient(c.Endpoints, c.Certfile, c.Keyfile, c.CAFile)
	} else {
		return etcd.NewClient(c.Endpoints), nil
	}
}

func (sub *EtcdSubnetRegistry) CheckEtcdIsAlive(seconds uint64) bool {
	for {
		status := sub.client().SyncCluster()
		log.Infof("Etcd cluster status: %v", status)
		if status {
			return status
		}
		if seconds <= 0 {
			break
		}
		time.Sleep(5 * time.Second)
		seconds -= 5
	}
	return false
}

func NewEtcdSubnetRegistry(config *EtcdConfig) (api.SubnetRegistry, error) {
	r := &EtcdSubnetRegistry{
		etcdCfg: config,
	}

	var err error
	r.cli, err = newEtcdClient(config)
	if err != nil {
		return nil, err
	}

	return r, nil
}

func (sub *EtcdSubnetRegistry) InitSubnets() error {
	key := sub.etcdCfg.SubnetPath
	_, err := sub.client().SetDir(key, 0)
	if err != nil {
		return err
	}
	key = sub.etcdCfg.SubnetConfigPath
	_, err = sub.client().SetDir(key, 0)
	return err
}

func (sub *EtcdSubnetRegistry) InitMinions() error {
	key := sub.etcdCfg.MinionPath
	_, err := sub.client().SetDir(key, 0)
	return err
}

func (sub *EtcdSubnetRegistry) GetMinions() (*[]string, error) {
	key := sub.etcdCfg.MinionPath
	resp, err := sub.client().Get(key, false, true)
	if err != nil {
		return nil, err
	}

	if resp.Node.Dir == false {
		return nil, errors.New("Minion path is not a directory")
	}

	minions := make([]string, 0)

	for _, node := range resp.Node.Nodes {
		if node.Key == "" {
			log.Errorf("Error unmarshalling GetMinions response node %s", node.Key)
			continue
		}
		_, minion := path.Split(node.Key)
		minions = append(minions, minion)
	}
	return &minions, nil
}

func (sub *EtcdSubnetRegistry) GetSubnets() (*[]api.Subnet, error) {
	key := sub.etcdCfg.SubnetPath
	resp, err := sub.client().Get(key, false, true)
	if err != nil {
		return nil, err
	}

	if resp.Node.Dir == false {
		return nil, errors.New("Subnet path is not a directory")
	}

	subnets := make([]api.Subnet, 0)

	for _, node := range resp.Node.Nodes {
		var s api.Subnet
		err := json.Unmarshal([]byte(node.Value), &s)
		if err != nil {
			log.Errorf("Error unmarshalling GetSubnets response for node %s: %s", node.Value, err.Error())
			continue
		}
		subnets = append(subnets, s)
	}
	return &subnets, err
}

func (sub *EtcdSubnetRegistry) GetSubnet(minionip string) (*api.Subnet, error) {
	key := path.Join(sub.etcdCfg.SubnetPath, minionip)
	resp, err := sub.client().Get(key, false, false)
	if err == nil {
		log.Infof("Unmarshalling response: %s", resp.Node.Value)
		var sub api.Subnet
		if err = json.Unmarshal([]byte(resp.Node.Value), &sub); err == nil {
			return &sub, nil
		}
		return nil, err
	}
	return nil, err
}

func (sub *EtcdSubnetRegistry) DeleteSubnet(minion string) error {
	key := path.Join(sub.etcdCfg.SubnetPath, minion)
	_, err := sub.client().Delete(key, false)
	return err
}

func (sub *EtcdSubnetRegistry) WriteNetworkConfig(network string, subnetLength uint) error {
	key := path.Join(sub.etcdCfg.SubnetConfigPath, "ContainerNetwork")
	_, err := sub.client().Create(key, network, 0)
	if err != nil {
		log.Warningf("Found existing network configuration, overwriting it.")
		_, err = sub.client().Update(key, network, 0)
		if err != nil {
			log.Errorf("Failed to write Network configuration to etcd: %v", err)
			return err
		}
	}

	key = path.Join(sub.etcdCfg.SubnetConfigPath, "SubnetLength")
	data := strconv.FormatUint(uint64(subnetLength), 10)
	_, err = sub.client().Create(key, data, 0)
	if err != nil {
		_, err = sub.client().Update(key, data, 0)
		if err != nil {
			log.Errorf("Failed to write Network configuration to etcd: %v", err)
			return err
		}
	}
	return nil
}

func (sub *EtcdSubnetRegistry) GetContainerNetwork() (string, error) {
	key := path.Join(sub.etcdCfg.SubnetConfigPath, "ContainerNetwork")
	resp, err := sub.client().Get(key, false, false)
	if err != nil {
		return "", err
	}
	return resp.Node.Value, err
}

func (sub *EtcdSubnetRegistry) GetSubnetLength() (uint64, error) {
	key := path.Join(sub.etcdCfg.SubnetConfigPath, "SubnetLength")
	resp, err := sub.client().Get(key, false, false)
	if err == nil {
		return strconv.ParseUint(resp.Node.Value, 10, 0)
	}
	return 0, err
}

func (sub *EtcdSubnetRegistry) CreateMinion(minion string, data string) error {
	key := path.Join(sub.etcdCfg.MinionPath, minion)
	_, err := sub.client().Get(key, false, false)
	if err != nil {
		// good, it does not exist, write it
		_, err = sub.client().Create(key, data, 0)
		if err != nil {
			log.Errorf("Failed to write new subnet to etcd: %v", err)
			return err
		}
	}

	return nil
}

func (sub *EtcdSubnetRegistry) CreateSubnet(minion string, subnet *api.Subnet) error {
	subbytes, _ := json.Marshal(subnet)
	data := string(subbytes)
	log.Infof("Minion subnet structure: %s", data)
	key := path.Join(sub.etcdCfg.SubnetPath, minion)
	_, err := sub.client().Create(key, data, 0)
	if err != nil {
		_, err = sub.client().Update(key, data, 0)
		if err != nil {
			log.Errorf("Failed to write new subnet to etcd: %v", err)
			return err
		}
	}

	return nil
}

func (sub *EtcdSubnetRegistry) WatchMinions(receiver chan *api.MinionEvent, stop chan bool) error {
	var rev uint64
	rev = 0
	key := sub.etcdCfg.MinionPath
	log.Infof("Watching %s for new minions.", key)
	for {
		resp, err := sub.watch(key, rev, stop)
		if err != nil && err == etcd.ErrWatchStoppedByUser {
			log.Infof("New subnet event error: %v", err)
			return err
		}
		if resp == nil || err != nil {
			continue
		}
		rev = resp.Node.ModifiedIndex + 1
		log.Infof("Issuing a minion event: %v", resp)
		minevent := newMinionEvent(resp.Action, resp.Node.Key, resp.Node.Value)
		receiver <- minevent
	}
}

func (sub *EtcdSubnetRegistry) watch(key string, rev uint64, stop chan bool) (*etcd.Response, error) {
	rawResp, err := sub.client().RawWatch(key, rev, true, nil, stop)

	if err != nil {
		if err == etcd.ErrWatchStoppedByUser {
			return nil, err
		} else {
			log.Warningf("Temporary error while watching %s: %v\n", key, err)
			time.Sleep(time.Second)
			sub.resetClient()
			return nil, nil
		}
	}

	if len(rawResp.Body) == 0 {
		// etcd timed out, go back but recreate the client as the underlying
		// http transport gets hosed (http://code.google.com/p/go/issues/detail?id=8648)
		sub.resetClient()
		return nil, nil
	}

	return rawResp.Unmarshal()
}

func (sub *EtcdSubnetRegistry) WatchSubnets(receiver chan *api.SubnetEvent, stop chan bool) error {
	for {
		var rev uint64
		rev = 0
		key := sub.etcdCfg.SubnetPath
		resp, err := sub.watch(key, rev, stop)
		if resp == nil && err == nil {
			continue
		}
		rev = resp.Node.ModifiedIndex + 1
		if err != nil && err == etcd.ErrWatchStoppedByUser {
			log.Infof("New subnet event error: %v", err)
			return err
		}
		subevent := newSubnetEvent(resp)
		log.Infof("New subnet event: %v, %v", subevent, resp)
		receiver <- subevent
	}
}

func (sub *EtcdSubnetRegistry) client() *etcd.Client {
	sub.mux.Lock()
	defer sub.mux.Unlock()
	return sub.cli
}

func (sub *EtcdSubnetRegistry) resetClient() {
	sub.mux.Lock()
	defer sub.mux.Unlock()

	var err error
	sub.cli, err = newEtcdClient(sub.etcdCfg)
	if err != nil {
		panic(fmt.Errorf("resetClient: error recreating etcd client: %v", err))
	}
}
