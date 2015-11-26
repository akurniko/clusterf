package clusterf

import (
    "fmt"
    "github.com/qmsk/clusterf/ipvs"
    "log"
    "syscall"
)

type ipvsType struct {
    Af          ipvs.Af
    Protocol    ipvs.Protocol
}

var ipvsTypes = []ipvsType {
    { syscall.AF_INET,      syscall.IPPROTO_TCP },
    { syscall.AF_INET6,     syscall.IPPROTO_TCP },
    { syscall.AF_INET,      syscall.IPPROTO_UDP },
    { syscall.AF_INET6,     syscall.IPPROTO_UDP },
}

type ipvsKey struct {
    Service     string
    Dest        string
}

type IpvsConfig struct {
    Debug       bool
    FwdMethod   string
    SchedName   string
}

type IPVSDriver struct {
    ipvsClient *ipvs.Client

    // global state
    routes      Routes

    // deduplicate overlapping destinations
    dests       map[ipvsKey]*ipvs.Dest

    // global defaults
    fwdMethod   ipvs.FwdMethod
    schedName   string
}

func (self IpvsConfig) setup(routes Routes) (*IPVSDriver, error) {
    driver := &IPVSDriver{
        routes: routes,
        dests:  make(map[ipvsKey]*ipvs.Dest),
    }

    if fwdMethod, err := ipvs.ParseFwdMethod(self.FwdMethod); err != nil {
        return nil, err
    } else {
        driver.fwdMethod = fwdMethod
    }

    driver.schedName = self.SchedName

    // IPVS
    if ipvsClient, err := ipvs.Open(); err != nil {
        return nil, err
    } else {
        log.Printf("ipvs.Open: %+v\n", ipvsClient)

        driver.ipvsClient = ipvsClient
    }

    if self.Debug {
        driver.ipvsClient.SetDebug()
    }

    if info, err := driver.ipvsClient.GetInfo(); err != nil {
        return nil, err
    } else {
        log.Printf("ipvs.GetInfo: version=%s, conn_tab_size=%d\n", info.Version, info.ConnTabSize)
    }

    return driver, nil
}

// Begin initial config sync by flushing the system state
func (self *IPVSDriver) sync() error {
    if err := self.ipvsClient.Flush(); err != nil {
        return err
    } else {
        log.Printf("ipvs.Flush")
    }

    return nil
}

func (self *IPVSDriver) newFrontend() *ipvsFrontend {
    return makeFrontend(self)
}

// bring up a service-dest with given weight, mergeing if necessary
func (self *IPVSDriver) upDest(ipvsService *ipvs.Service, ipvsDest *ipvs.Dest, weight uint32) (*ipvs.Dest, error) {
    ipvsKey := ipvsKey{ipvsService.String(), ipvsDest.String()}

    if mergeDest, mergeExists := self.dests[ipvsKey]; !mergeExists {
        ipvsDest.Weight = weight

        log.Printf("clusterf:ipvs upDest: new %v %v\n", ipvsService, ipvsDest)

        if err := self.ipvsClient.NewDest(*ipvsService, *ipvsDest); err != nil {
            return ipvsDest, err
        }

        self.dests[ipvsKey] = ipvsDest

        return ipvsDest, nil

    } else {
        log.Printf("clusterf:ipvs upDest: merge %v %v +%d\n", ipvsService, mergeDest, weight)

        mergeDest.Weight += weight

        if err := self.ipvsClient.SetDest(*ipvsService, *mergeDest); err != nil {
            return mergeDest, err
        }

        return mergeDest, nil
    }
}

// update an existing dest with a new weight
func (self *IPVSDriver) adjustDest(ipvsService *ipvs.Service, ipvsDest *ipvs.Dest, weightDelta int) error {
    ipvsKey := ipvsKey{ipvsService.String(), ipvsDest.String()}

    if mergeDest := self.dests[ipvsKey]; mergeDest != ipvsDest {
        panic(fmt.Errorf("invalid dest %#v should be %#v", ipvsDest, mergeDest))
    }

    ipvsDest.Weight = uint32(int(ipvsDest.Weight) + weightDelta)

    // reconfigure active in-place
    if err := self.ipvsClient.SetDest(*ipvsService, *ipvsDest); err != nil  {
        return err
    }

    return nil
}

// bring down a service-dest with given weight, merging if necessary
func (self *IPVSDriver) downDest(ipvsService *ipvs.Service, ipvsDest *ipvs.Dest, weight uint32) error {
    ipvsKey := ipvsKey{ipvsService.String(), ipvsDest.String()}

    if mergeDest := self.dests[ipvsKey]; mergeDest != ipvsDest {
        panic(fmt.Errorf("invalid dest %#v should be %#v", ipvsDest, mergeDest))
    }

    if ipvsDest.Weight > weight {
        log.Printf("clusterf:ipvs downDest: merge %v %v -%d\n", ipvsService, ipvsDest, weight)

        ipvsDest.Weight -= weight

        if err := self.ipvsClient.SetDest(*ipvsService, *ipvsDest); err != nil {
            return err
        }

    } else if ipvsDest.Weight < weight {
        panic(fmt.Errorf("invalid weight %d for dest %#v", weight, ipvsDest))

    } else {
        log.Printf("clusterf:ipvs downdest: del %v %v\n", ipvsService, ipvsDest)

        if err := self.ipvsClient.DelDest(*ipvsService, *ipvsDest); err != nil  {
            return err
        }

        delete(self.dests, ipvsKey)
    }

    return nil
}

func (self *IPVSDriver) clearService(ipvsService *ipvs.Service) {
    for ipvsKey, _ := range self.dests {
        if ipvsService.String() == ipvsKey.Service {
            delete(self.dests, ipvsKey)
        }
    }
}

func (self *IPVSDriver) Print() {
    if services, err := self.ipvsClient.ListServices(); err != nil {
        log.Fatalf("ipvs.ListServices: %v\n", err)
    } else {
        fmt.Printf("Proto                           Addr:Port\n")
        for _, service := range services {
            fmt.Printf("%-5v %30s:%-5d %s\n",
                service.Protocol,
                service.Addr, service.Port,
                service.SchedName,
            )

            if dests, err := self.ipvsClient.ListDests(service); err != nil {
                log.Fatalf("ipvs.ListDests: %v\n", err)
            } else {
                for _, dest := range dests {
                    fmt.Printf("%5s %30s:%-5d %v\n",
                        "",
                        dest.Addr, dest.Port,
                        dest.FwdMethod,
                    )
                }
            }
        }
    }
}
