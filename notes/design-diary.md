# Design Diary

## 4/16/21  

* Working switching from 3 tracks to N (say, 100) tracks.
* Also considering the goroutine design.
* Ideally one goroutine per core or per subscriber for sending
* Per-subscriber state should only be touched by a single goroutine
* RX media is sent to N channels, where N is the number of goroutines, either num-cores or num-subs

Notes on gr-per-core:
* all media goes to all cores
* subs must be assigned to a single core
* no rx media filtering needed before send to chan
* may have advantages when moving to openssl/xdp
* the approach a salty C programmer would use
* subscriber state is independent of each other, so design is simple loop

Notes on gr-per-sub:
* dont necessarily want all media to all subs, pre-chan filtering might be optimal
* switching channels can be done with a message vs mutex/lock

What about gr-per-rx-track-per-core ?
* interesting
* open ssl loop
* then xdp it away

What about gr-per-rx-track ?
* NO
* basically what we have now
* the main issue, is that if we have one rx-track, all sending activity happens on a single GR, meaning you are not using your fancy 48 core CPU
* might mean multiple goroutines are contending for subscriber state, though :(, ie when switching channels

how does channel switch occur?
* happens on http handler goroutine

## 4/19/21 Notes on multi-core sending

* X tracks of inbound media (ie, 4 = 3 video, 1 audio)
* x is a particular track
* Y tracks of outbound media (ie, 10 sfus + 10 browsers = 40 + 20 = 60)
* workers waiting on Rx media and passing to Tx tracks seems right
* SFUs are simple as the rx->tx assignments are fixed for the life of the connection
* Browsers are not so simple to rx->tx assigments can change as the browser can change tracks
* inbound media must maintain ordering on the way to tx tracks (avoid races)
* would like to avoid mutexs that could contend, and in general. (can't avoid pion mutexes)
* switching between two rx tracks requires a mutex or a central goroutine. I choose central goroutine
* workers need to iterate sub's to send media OR contain/hide TX sub info. I choose contain.
* the main design issue/trickiness is that TX tracks can switch between different RX tracks, of which the timing of is conditioned on the incoming/pending RX track media packet being 'switchable'
* the conditioning is not the hard part
* the hard part is the migrating between tx-workers
* maybe its not so hard. we have a 'center' GR (goroutine)
* this is required in order to do switching without using a mux (mux or central is required due to ordered media requirement)
* the 'center' GR is controls switching
* when the Center determines and rtp. Packet is switchable+a track needs to be switched, 
* it sends a 'stop-sending' sentinel to all workers and sends a 'start-sending' sentinal to the proper worker. thats all that is required if the workers don't need to touch mutex-shared-state related to the TX track!!!

Scenario 3rx chan, 10000tx chan all fed from rx#2
we would we not want all 10000tx chans to be fed on the same GR
SO, A SINGLE GR per RX does not work
must have multiple sendGR per RX

## 4/20/21 move WriteRTP off of OnIngress Goroutine, plus lift 3 RX track limit

* a single center gr would need to iterate the pending-change set in order

* there are two kinds of TX tracks: switchable and non-switchable

## 4/21/21

* observation about PC. AddTrack()
* if you add a dozen, or 1000 tracks using PC. AddTrack(), and some of those PC's close/goaway. that track won't be writing to a dozen or 1000 PC-tracks, but something less. so if you need to know how many sub-writes a track. WriteRTP() will cause, tracking/predicting that becomes tricky. (you can somehome watch for closing PCs, but this will look gross IMO)
* this may support creating a new track for each SFU track rather than using a 'shared' track

## 4/21/21 redesign thoughts

* txid is a 16 character random hex string (64 bits)
* zero shared state between xmain and http handlers / no mutexes
* zero shared tracks between Peerconns. no shared tracks: no shared audio, no shared video
* downstream SFU and Browser share the same implementations for sending [wow!]
* subHandler does NOT keep a subscriber map/array (wow) (channel-change goes straight to xmain)

### xmain purpose and messages

* xmain is a func/goroutine which accepts packets and forwards packets directly or to workers.
* xmain does not call pion, ever, or other mysterious methods.
* xmain might be thought of as the media controller gr/method
* on new subscriber, subhandler sends txid+array of tracks to xmain
* on track change, subHandler passes txid and new track integer to xmain to handle
* idleloop sends packets to xmain
# 4/22/21 working on: moving TXing off of RX goroutines
* no more shared tracks, ie: peercon1. AddTrack(X)  peercon2. AddTrack(X)
* no mutexes? well, just lightly contended mutexes. lol

## 5/1/21 idle handling

* idle detection (when is an RX track missing/gone/idle) needs to be periodically done
* it could be done on the RX media event, with support from a periodic timer 
* seems cleaner to solely use a periodic timer to do the switching
* but the marking of last RX time needs to happen on RX media, of course.
* So on new media we update the time.

## 5/3/21 fixing Track type cleanup

## 5/3/21 The subscriber Browser or SFU controls the number of video+audio transcievers

## 5/10/21 To use struct indexes or 'int' indexes

* ultimatly there are audio/video tracks and regular-rx vs idle-rx tracks
* I realized that I hadn't included 'idle' assignments in the Audio0, Video0 style enums
* I have explored moving away from enums to a struct for the source or truth of track indexes:
struct {

	index        int
	isAudio      bool
	isIdleSource bool

}
var uniqRxid map[RxTxId]Rxid = make(map[RxTxId]Rxid)
var uniqTxid map[RxTxId]Txid = make(map[RxTxId]Txid)

* While this works and provides a ton of flexibility down the road, I think it is overkill
* So, I will save this work but use git-revert to move back to int Video0 style enum indexes
* It is a little less flexible, but so much simpler in the long run.

## 5/17/21 Looking at Media Forwarding Engine Again

I see two major ways of doing multi-core pkt forwarding:
A. *Unsynchronized-writers*: Using goroutines, and channels, RX packet order is maintained toward TX writes, and switching is also implemented.
B. *One-RX-pkt-at-time*: all cores are let loose on one packet and a TX track list and the pkts is forwarded to all TX tracks until there is noting left to Write(). This is done in a loop for each packet.

The main question between the two, is "how is packet ordering maintained".
In method #B, it is simple, one packet is worked on until all Write()s are complete, all GR are waited for done/ready and another packet is worked upon.
In method #A, you need to maintain the channel/GR packet path from RX to TX. And for switching there are two primary methods: and either have a single GR switching between changing RX, 

### Method A/*Unsynchronized-writers*

GR=goroutine, pkt=packet
Method A is simple and has advantages in systems with no switching, but in a system with switching like
ours it might become complicated. 
We can't send from the receiving OnTrack goroutine, we need to be able to use multiple GR for large Subscribers counts.
So, the OnTrack/GR will send to one or many Senders.
Those Senders will be married to a particular track in order to maintain pkt ordering.
So far so good.
When switching a Sender will either recv msg to switch, or read a struct-flag in memory.
When a keyframe/switchpoint occurs the Sender it will change it's unshared state for the Track
to the new/pending Rxid.

All RX packets will need to be transmitted to all senders.
*The main issue with this approach is work distribution*
*webrtc. LocalTracks are tied to GRs, so getting num tracks per GR sizing correct is important and not flexible. Also, all Tracks tied to a GR could have different Rxid*
* Every GR for this approach would have to receive every RX packet. This might not be a big cost. The ratio of RX-work to TX-work should be very low. ie: rxwork/txwork < .0001 for example.
* This approach requires either a) a shared Tracks slice, or b) messaging the Senders to inform them of their Track list. *both of these are ugly*

### Method B/*One-RX-pkt-at-time*

The big advantage of method B, is that there is the "classic single-thread/GR" that maintains most relevant state. With little or no shared state.

## 5/18/21 media engine design notes

Simplest design:
One-slice of Tracks, Track includes 'pending Txid' and 'active bool'
Can iter 1e10 48-byte structs in 155us, or 1.5us per 100-core box
For max packet throughput of <1e6 or ~.66e6 pps

## 5/30/21 media sending engine/switching engine

I am not satisfied with my current designs for multi-goroutine sending/switching engine.
I feel it is too complex.

I am going back to basics.

Simple idea #1
* slice of all tracks, Track contains txid, pendingid, mutex, etc
* multiple GRs scan tracks-slice, and lock, switch and send on each Track

Simple idea #2
* Two slices: stuct{active, pending Txid}, and Track
* GRs use atomic. AddInt32 to grab chunks of the first slice and work on their chunks

Simple idea #3
* One or more slices of components of Track, maybe active Txid, pending Txid have own slices.
* GRs use atomic. AddInt32 to grab ranges of the slices, and to work on their range.
* chans+GRs could be used as an alternative to range-grabbing, but this might be less granular, as each Track must be fixed to a GR

Down the road: A 'map[int]int'  for counts of a Txid in a range of the indexes can LATER be
used to skip chunks of the slice(s).

This example can be helpful: https://gobyexample.com/worker-pools

Simple approach recap:
* single slice of Track structs
* Track contains 'source Rxid' and 'pending Rxid' (literally or in same-indexed slice)
- 

Big recap of major questions for media/switching engine:
* single slice with encapsulated sourcerxid/pendingrxid vs. txslice/txmap or pendingslice/pendingmap
  + we are moving toward: encapsulated sourcerxid/pendingrxid with effective single-slice
* packet sending: fire-and-forget vs send-all-wait-for-completion
  + fire-and-forget requires packets to always go to the same channel/GR to maintain ordering
  + so we choose syncronous send-and-wait approach

## 6/18/21 Handing Usage from private-ip only situations

When deadsfu detects the default route/interface only has private IPv4 addresses, this means:
* The system is or is not on the Internet, we don't know
* If it is on the Internet, it will have a public-IP, which is brittle to detect.
* We also, don't want to detect the public-IP.
- 

In this situation (only private IPv4 addrs), we want the user to clarify whether they
want the system to be accessed via it's private-ip, or presumable public-ip.
There is involved steps we could do to detect an open public-ip port, but lets keep it simple for now.

So, we have two flags: '-public: for the DNS hostname, deadsfu will detect and register public IP-addresses'
and '-private: for the DNS hostname, deadsfu will detect and register the private IP-addresses'

If the SFU only has private IP then we require the user to indicate whether the hostname should use the private IP or the public IP.
If the user.indicates public, then we query a service like xxx.aws.com to find their public IP.
With regards to helping the user discover the openness their ip, 

## 6/27/21 We do not provide -https-auto auto

I was thinking about four options for -https-auto: none, public, local, *and auto*
Where 'auto' would decide automatically betwen local and public.
But how much value vs complexity does it really offer?
After thinking it through, not enough value in exchange for complexity.
*So I am killing -https-auto auto*

## 6/27/21 Should we do our own 'port 80 open' and 'port 443 open' checks??

We could check for the openness of port 80 and 443 prior to invoking certmagic.
This does have the advantage of more easily providing explicit messaging about
a fatal condition.
*The downside is* everything becomes .2 to 2.0 seconds slower for everybody. :(
*One upside is* our checks will probably be much faster than failure detected by LE.
*Decision* Let's code for both, and decide based upon empirical experiece.

## 6/27/21 Internet based open port checking

Internet based open port checking is complex:
* minus: we need to run and pay for a DO server running a proxy forever
* minus: certmagic can successfully challenge using port 80, even when we the end-user code is not running an http server on port 80.
* minus: to check port 80, we need to run a server and not conflict with certmagic
* point: using timers and observing certmagic events, we can report whether a cert has been aquired or not, given the elapsed time. this messaging, while not be certain about port issues, can raise the issue for the end-user

*Decision* We kill the proxy, and the deadsfu proxy code, and take down the socks5 proxy.

## 6/27/21 More on Open Port Checking and HTTP-Bound and HTTPS-Bound Checking

*Decision* We no longer do any open port checking.
*Decision* We no longer check the ports of the Http URL, nor HTTPS URL to report whether LetsEncrypt will fail. Letsencrypt and certmagic can still pass challenges with https not on 443, and http not on 80.

## 6/28/21 Whether to use dns01 or port80/443 challenges

As we are observing today on AWS-Lightsail/Ubuntu, there are two major issues with port80/443 challenges:
1) you need root 2) may need to tweak the firewall for 80/443
1. You often must be root to bind and listen on 443/80
2. You sometimes have go tweak the firewall. (If you want to run on 8443, you still need to tweak the firewall to get port 80||443 open, for the 80/443 challenges)

*Decision* We will default to DNS01 challenge for both -https-auto public and local.

## 7/27/21 Do not automatically redirect http->https unless a flag is set

Don't automatically redirect http requests to https, as we have done, 
as it appears from using k8s, that having both http+https handling SDPs+html
can make sense.
We will add a flag.

## 7/29/21 k3s/k8s ingress of http/s WHIP and WASH on k3s

on k8s/k3s we have two 'ingress' spots for http/s:
* for the publisher
* for the subscribers

Doing the publisher ingress kind already works, because of the zero-conf hostname+ip+dns01 challenge of deadsfu
But! subscriber egress does not work: because the many subscriber-pods are accessed via many http points.
(the many deadsfu pods are accessed via http)
So...., we really need a dead-simple way to give devs a zero-conf https experience into the subscribers.
We could:
* offer Http into the subscribers is easy, but everyone would balk at that.
* write a simple deadsfu-like ingress container/proxy/lb (you know dnsregister()...)
* write a k3s/k8s Ingress Controller, ala: [Caleb Doxsey][1]
* fix traefik to have a Lego with ddns5.com support

[1]: https://www.doxsey.net/blog/how-to-build-a-custom-kubernetes-ingress-controller-in-go
[2]: https://github.com/calebdoxsey/kubernetes-simple-ingress-controller
[3]: https://dgraph.io/blog/post/building-a-kubernetes-ingress-controller-with-caddy/
[4]: https://github.com/dgraph-io/ingressutil
[5]: https://www.digitalocean.com/community/questions/use-kubernetes-without-a-load-balancer?answer=57547
[6]: https://github.com/ebrianne/cert-manager-webhook-duckdns
[7]: https://serverfault.com/a/869453/114731

## 7/31/21 ways to use ddns5 to get ingress into k8s

1. write a simple deadsfu-like ingress container/proxy/lb (you know dnsregister()...)::doesn't fix firewall
2. write a k3s/k8s Ingress Controller, ala: [Caleb Doxsey][1] [Github][2] [Tejas Dinkar][3] [github][4]:: requires loadbalancer$$
3. get Service/NodePort working. not recommended by DO people: [nodeport changes IP][5], not working for me either.OPENS FW
4. fork/fix traefik+lego to use ddns5. good for public ip, but private IP?? :: requires loadbalancer$$
5. get Server/LoadBalancer working somehow, which should work fine :: requires loadbalancer$$
6. use certificatemanager with a webhook for ddns5 [github][6] :: requires loadbalancer$$
7. use Service + External IP [stackoverflow][7] :: maybe
8. use hostnetwork if node has public IP+no firewall (k3s)::doesn't fix firewall

in the long run everybody wanna use cert-manager plus service/loadbalancer for big deploys:
in this case the root is http-IN
in the long run everybody prolly want use certmanager plus service/NodePort for cheapo deploys
in this case the root is http-IN

decisions:
the root deadsfu node will use http in, not https
https/tls termination is handled by an k8s ingress controller, not by deadsfu
cheapo ingress will be done via Service/NodePort
high-end ingress will be done via Service/LoadBalancer
*we need to fork [this][6] and do a version for ddns5*

alternate:
still do https into deadsfu
find IP address via stun
expose using either nodeport or loadbalancer(nope)
*this should be avoided, because we switch between https/http for trial/production*

WOW: do NOT mix: hostNetwork: true and NodePort!!!!

Can/should we get IP address via Stun???
Can we eliminate the --ddns-public flag??
maybe create --stunserver-or-ipaddr??
maybe --my-ipaddr <address> stunserver or local or public
--z-debug dumps all ip addresses first thing

## 8/4/2021 major hack attack

## 8/12/2021 notes on rxidstate

// this (rxidstate) could be an array.
// but, it is <much> easier to think about as a map, as opposed to a sparse array.
// AND, this map is not indexed in any hot-spots or media-paths
// so, there is NO good reason to make it an array
// sooo... we keep it a map.

# 8/14/21 decided to gut multi-track support

# 8/16/21 need a tool for ES (elementary stream) consistency checking in the field

- for either run-time or:
- post run-time consistency checking

# 8/18/21

screen flow text:
DeadSFU: Use --idle-screen to replace this screen
No Input Present
