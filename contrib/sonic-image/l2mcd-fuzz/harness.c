/*
 * harness.c -- drive l2mcd's own IGMP and MLD receive paths in-process under
 * AddressSanitizer and UndefinedBehaviorSanitizer: a regression runner (main:
 * one named case per finding) and two libFuzzer targets (-DFUZZ_IGMP,
 * -DFUZZ_MLD) whose input is a whole IPv4 or IPv6 packet.
 *
 * The daemon is set up the way it runs: its init functions, then the IPC
 * messages l2mcmgrd sends (port list, snooping config for IPv4 and IPv6 with
 * the VLAN's members, VLAN membership) through its own l2mcd_process_ipc_msg(),
 * then an IGMPv3 and an MLDv2 general query on Ethernet4 so that port is a
 * router port and reports get forwarded (l2mcd_fwd_pkt). Packets then go in
 * through receive_igmp_packet() / receive_mld_packet() built exactly as the
 * socket handlers in l2mcd_system.c build them, each in a heap buffer of
 * exactly the received size, so any read or write past the packet is a report.
 * What the handlers check before that is mirrored here: the fixed tree's
 * l2mcd_igmp_rx_len_ok() when the patched header declares it, else nothing
 * (the unfixed handler trusts the header). Only the APPL_DB sync layer
 * (l2mcd_sync.cpp) is stubbed (stubs.c); transmits are captured, not sent.
 *
 * The ports must exist as interfaces (port setup asks the kernel): run it in
 * a network namespace with dummy links (run.sh does that), never on a host
 * with real ports by these names.
 */
#include "port/l2mcd_system.c"     /* for the static IPC dispatcher (-I <src>) */
#include <sanitizer/lsan_interface.h>

#define VID 100
static const char *ports[] = { "Ethernet4", "Ethernet8" };
static mld_vlan_node_t *vnode;
static unsigned int port_idx[2];        /* l2mcd's port index per port */
static size_t tx_count, tx_bytes;       /* what l2mcd_fwd_pkt would have sent */
static uint32_t tx_igmp_src;            /* source address of the last IGMP frame sent */
static int tx_igmp_type = -1;           /* and its IGMP type */
static int addr_order;                  /* query-src cases: Vlan100's address 1 before, 2 after the snooping enable */
#define VLAN_ADDR "192.0.2.200"
#define VLAN_ADDR_U32 0xc00002c8u

ssize_t __real_sendto(int, const void *, size_t, int, const struct sockaddr *, socklen_t);
ssize_t __wrap_sendto(int fd, const void *buf, size_t len, int flags,
                      const struct sockaddr *to, socklen_t tolen)
{
    /* packet transmits are counted and dropped; the IPC socket is not used */
    const uint8_t *b = buf;
    size_t off;

    (void)fd; (void)flags; (void)to; (void)tolen;
    tx_count++;
    tx_bytes += len;
    /* an untagged or 802.1Q-tagged IPv4 frame carrying IGMP: keep its source */
    for (off = 12; off <= 16 && off + 2 + 24 <= len; off += 4) {
        const uint8_t *ip = b + off + 2;
        if (b[off] != 0x08 || b[off + 1] != 0x00)
            continue;
        if (ip[9] == 2 && (size_t)(off + 2 + (ip[0] & 0x0f) * 4) < len) {
            tx_igmp_src = (uint32_t)ip[12] << 24 | ip[13] << 16 | ip[14] << 8 | ip[15];
            tx_igmp_type = ip[(ip[0] & 0x0f) * 4];
        }
        break;
    }
    return len;
}

/* l2mcd logs through syslog, which nothing receives here: with HARNESS_DEBUG
 * its lines go to stderr instead */
void __real_vsyslog(int, const char *, va_list);
void __wrap_vsyslog(int pri, const char *fmt, va_list ap)
{
    (void)pri;
    if (getenv("HARNESS_DEBUG")) {
        vfprintf(stderr, fmt, ap);
        fputc('\n', stderr);
    }
}

static void cfg(int type, int afi, int version, int opcode, int nports)
{
    size_t dlen = sizeof(L2MCD_CONFIG_MSG) + nports * sizeof(PORT_ATTR);
    L2MCD_IPC_MSG *m = calloc(1, sizeof(L2MCD_IPC_MSG) + dlen);
    L2MCD_CONFIG_MSG *c = (L2MCD_CONFIG_MSG *)m->data;
    struct sockaddr_un none;
    int i;

    memset(&none, 0, sizeof(none));
    m->msg_type = type;
    m->msg_len = dlen;
    c->op_code = opcode;
    c->enabled = 1;
    c->afi = afi;
    c->version = version;
    c->query_interval = 125;
    c->last_member_query_interval = 1000;
    c->query_max_response_time = 10;
    c->vlan_id = VID;
    c->count = nports;
    for (i = 0; i < nports; i++) {
        strncpy(c->ports[i].pnames, ports[i], L2MCD_IFNAME_SIZE - 1);
        c->ports[i].oper_state = 1;
    }
    l2mcd_process_ipc_msg(m, sizeof(L2MCD_IPC_MSG) + dlen, none);
    free(m);
}

/* Vlan100's address as l2mcmgrd sends it from INTERFACE_TABLE (Vlan100|<addr>/24) */
static void vlan_addr(void)
{
    size_t dlen = sizeof(L2MCD_CONFIG_MSG);
    L2MCD_IPC_MSG *m = calloc(1, sizeof(L2MCD_IPC_MSG) + dlen);
    L2MCD_CONFIG_MSG *c = (L2MCD_CONFIG_MSG *)m->data;
    struct sockaddr_un none;

    memset(&none, 0, sizeof(none));
    m->msg_type = L2MCD_INTERFACE_TABLE_UPDATE;
    m->msg_len = dlen;
    c->op_code = L2MCD_OP_ENABLE;
    c->afi = MLD_IP_IPV4_AFI;
    c->vlan_id = VID;
    c->prefix_length = 24;
    strncpy(c->gaddr, VLAN_ADDR, L2MCD_IP_ADDR_STR_SIZE - 1);
    l2mcd_process_ipc_msg(m, sizeof(L2MCD_IPC_MSG) + dlen, none);
    free(m);
}

static uint16_t csum16(const uint8_t *p, size_t n, uint32_t sum)
{
    for (; n > 1; p += 2, n -= 2)
        sum += (p[0] << 8) | p[1];
    if (n)
        sum += p[0] << 8;
    while (sum >> 16)
        sum = (sum & 0xffff) + (sum >> 16);
    return (uint16_t)~sum;
}

/* ---- IPv4 ---- */

/* Make the IGMP checksum right over [ihl*4, tot_len) when those bytes exist,
 * as a sender would. */
static void fix_igmp_csum(uint8_t *pkt, size_t len)
{
    size_t hl, tl;
    uint16_t c;

    if (len < 20)
        return;
    hl = (pkt[0] & 0x0f) * 4;
    tl = (pkt[2] << 8) | pkt[3];
    if (hl < 20 || tl > len || tl < hl + 4)
        return;
    pkt[hl + 2] = pkt[hl + 3] = 0;
    c = csum16(pkt + hl, tl - hl, 0);
    pkt[hl + 2] = c >> 8;
    pkt[hl + 3] = c & 0xff;
}

/* One IPv4 packet as l2mcd_recv_igmp_msg() hands it on, arriving on port p. */
static void feed_ip4(int p, const uint8_t *data, size_t len)
{
    IP_PARAMETERS ip;
    uint8_t *pkt;

    if (len < 20)       /* the handler reads a 2 KB slot: no heap overrun to see */
        return;
#ifdef L2MCD_IGMP_RX_LEN_CHECK
    if (!l2mcd_igmp_rx_len_ok(data, len)) {     /* as l2mcd_recv_igmp_msg() does */
        l2mcd_rx_drop(L2MCD_IPV4_AFI, VID, port_idx[p], "IPv4 header lengths past the bytes received");
        return;
    }
#endif
    pkt = malloc(len);
    memcpy(pkt, data, len);
    memset(&ip, 0, sizeof(ip));
    ip.rx_port_number = vnode->ifindex;
    ip.rx_phy_port_number = port_idx[p];
    ip.rx_vlan_id = VID;
    memcpy(&ip.source_address, pkt + 12, 4);
    memcpy(&ip.destination_address, pkt + 16, 4);
    ip.header_length = (pkt[0] & 0x0f) * 4;
    ip.total_length = (pkt[2] << 8) | pkt[3];
    ip.time_to_live = pkt[8];
    ip.vrf_index = L2MCD_DEFAULT_VRF_IDX;
    ip.data = pkt;
    receive_igmp_packet(&ip);
    free(pkt);
}

/* Build an IPv4 packet around an IGMP message: 24-byte header with router
 * alert, tot_len = claim (0: the real length), checksum made valid over the
 * bytes present. Returns the packet length. */
static size_t mk_ip4(uint8_t *pkt, const uint8_t *msg, size_t mlen, size_t claim,
                  const uint8_t dst[4])
{
    const size_t hl = 24;
    size_t total = hl + mlen;
    uint16_t c;

    memset(pkt, 0, hl);
    pkt[0] = 0x46;
    pkt[2] = (claim ? claim : total) >> 8;
    pkt[3] = (claim ? claim : total) & 0xff;
    pkt[8] = 1;
    pkt[9] = IPPROTO_IGMP;
    pkt[12] = 192; pkt[13] = 0; pkt[14] = 2; pkt[15] = 10;      /* 192.0.2.10 */
    memcpy(pkt + 16, dst, 4);
    pkt[20] = 0x94; pkt[21] = 0x04;
    memcpy(pkt + hl, msg, mlen);
    if (mlen >= 4) {
        pkt[hl + 2] = pkt[hl + 3] = 0;
        c = csum16(pkt + hl, mlen, 0);
        pkt[hl + 2] = c >> 8;
        pkt[hl + 3] = c & 0xff;
    }
    return total;
}

static const uint8_t v3_reports[4] = { 224, 0, 0, 22 };
static const uint8_t all_hosts[4] = { 224, 0, 0, 1 };

/* ---- IPv6 ---- */

/* Make the ICMPv6 checksum right when the header chain lets us find it. */
static void fix_mld_csum(uint8_t *pkt, size_t len)
{
    size_t off, plen, mlen, i;
    uint32_t sum = 0;
    uint16_t c;

    if (len < 40)
        return;
    plen = (pkt[4] << 8) | pkt[5];
    if (40 + plen > len)
        return;
    if (pkt[6] == IPPROTO_HOPOPTS) {
        if (plen < 2)
            return;
        off = 40 + (pkt[41] + 1) * 8;
    } else if (pkt[6] == IPPROTO_ICMPV6) {
        off = 40;
    } else {
        return;
    }
    if (off + 4 > 40 + plen)
        return;
    mlen = 40 + plen - off;
    pkt[off + 2] = pkt[off + 3] = 0;
    for (i = 8; i < 40; i += 2)
        sum += (pkt[i] << 8) | pkt[i + 1];
    sum += mlen + IPPROTO_ICMPV6;
    c = csum16(pkt + off, mlen, sum);
    pkt[off + 2] = c >> 8;
    pkt[off + 3] = c & 0xff;
}

/* One IPv6 packet as l2mcd_recv_mld_msg() hands it on, arriving on port p:
 * the handler's own checks first (length floor, next header 0 or 58). */
static void feed_ip6(int p, const uint8_t *data, size_t len)
{
    IP6_RX_PKT_MSG m;
    uint8_t *pkt;

    if (len < sizeof(IPV6_HEADER) + 2 + sizeof(ICMP6_PSEUDO_HDR_MESSAGE) || len > 2034)
        return;
    if (data[6] != IPPROTO_HOPOPTS && data[6] != IPPROTO_ICMPV6)
        return;
    pkt = malloc(len);
    memcpy(pkt, data, len);
    memset(&m, 0, sizeof(m));
    m.ip_param.rx_port_number = vnode->ifindex;
    m.ip_param.rx_physical_port_number = port_idx[p];
    m.ip_param.vrf_index = L2MCD_DEFAULT_VRF_IDX;
    m.ip_param.vlan_id = VID;
    m.ip_param.hop_limit = pkt[7];
    m.ip_param.version = pkt[0] >> 4;
    m.ip_param.next_header = pkt[6];
    m.ip_param.payload_length = (pkt[4] << 8) | pkt[5];
    memcpy(&m.ip_param.source_address, pkt + 8, 16);
    memcpy(&m.ip_param.destination_address, pkt + 24, 16);
    m.pkt_data = pkt;
    m.pkt_size = len;
    receive_mld_packet(&m);
    free(pkt);
}

/* Build an IPv6 packet around an MLD message: 8-byte hop-by-hop header with
 * router alert (hbh 0: none, next header ICMPv6), payload_length = claim
 * (0: the real length), checksum made valid. */
static size_t mk_ip6(uint8_t *pkt, const uint8_t *msg, size_t mlen, int hbh, size_t claim,
                  uint8_t dst_last)
{
    size_t hl = hbh ? 48 : 40;
    size_t total = hl + mlen;

    memset(pkt, 0, hl);
    pkt[0] = 0x60;
    pkt[4] = (claim ? claim : total - 40) >> 8;
    pkt[5] = (claim ? claim : total - 40) & 0xff;
    pkt[6] = hbh ? IPPROTO_HOPOPTS : IPPROTO_ICMPV6;
    pkt[7] = 1;
    pkt[8] = 0xfe; pkt[9] = 0x80; pkt[23] = 0x10;               /* fe80::10 */
    pkt[24] = 0xff; pkt[25] = 0x02; pkt[39] = dst_last;         /* ff02::N */
    if (hbh) {
        pkt[40] = IPPROTO_ICMPV6; pkt[41] = 0;
        pkt[42] = 5; pkt[43] = 2; pkt[44] = 0; pkt[45] = 0;     /* router alert, MLD */
        pkt[46] = 1; pkt[47] = 0;                               /* PadN */
    }
    memcpy(pkt + hl, msg, mlen);
    fix_mld_csum(pkt, total);
    return total;
}

static int setup_daemon(void);

/* What the daemon allocates while being configured is its own business (it
 * drops some of it); LeakSanitizer counts only what packets allocate. */
static int setup(void)
{
    static int done, rc;

    if (done)
        return rc;
    done = 1;
    __lsan_disable();
    rc = setup_daemon();
    __lsan_enable();
    return rc;
}

static int setup_daemon(void)
{
    static uint8_t b[256], q[64];
    size_t n;
    APP_LOG_INIT();
    APP_LOG_SET_LEVEL(getenv("HARNESS_DEBUG") ? APP_LOG_LEVEL_DEBUG : APP_LOG_LEVEL_EMERG);
    if (getenv("HARNESS_LOGARGS") && *getenv("HARNESS_LOGARGS")) {    /* evaluate every per-VLAN log argument, into this file */
        g_l2mcd_pkt_fp = fopen(getenv("HARNESS_LOGARGS"), "w");
        memset(g_l2mcd_pkt_log, 1, sizeof(g_l2mcd_pkt_log));
        g_l2mcd_vlan_log_mask = L2MCD_LOG_MASK_INFO | L2MCD_LOG_MASK_DEBUG | L2MCD_LOG_MASK_PKT;
    }
    if (mld_vdb_init() < 0)
        return -1;
    mcast_igmp_init();
    mcast_mld_init();
    l2mcd_avll_init();
    portdb_init();
    mld_portdb_gvid_hash_init();
    port_ifname_db_init();
    l3_time_freq_init();
    /* as l2mcd_system_init(): an event base with its two priorities (without
     * them every per-port socket fails to register and is dropped), and the
     * per-port receive sockets AF_PACKET */
    g_l2mcd_evbase = event_base_new();
    event_base_priority_init(g_l2mcd_evbase, L2MCD_LIBEV_PRIO_QUEUES);
    g_l2mcd_rx_is_l2_sock = TRUE;

    cfg(L2MCD_SNOOP_PORT_LIST_MSG, L2MCD_IPV4_AFI, 0, 1, 2);
    if (addr_order == 1)
        vlan_addr();
    cfg(L2MCD_SNOOP_CONFIG_MSG, L2MCD_IPV4_AFI, 3, 1, 2);
    cfg(L2MCD_SNOOP_CONFIG_MSG, L2MCD_IPV6_AFI, 2, 1, 2);
    cfg(L2MCD_VLAN_MEM_TABLE_UPDATE, L2MCD_IPV4_AFI, 0, 1, 2);
    if (addr_order == 2)
        vlan_addr();

    vnode = mld_vdb_vlan_get(VID, MLD_VLAN);
    port_idx[0] = portdb_get_portindex_from_ifname((char *)ports[0]);
    port_idx[1] = portdb_get_portindex_from_ifname((char *)ports[1]);
    if (!vnode || port_idx[0] == NO_SUCH_PORT || port_idx[1] == NO_SUCH_PORT ||
        /* the receive paths index port_list by the ifindex cut to 16 bits (USHORT) */
        !gIgmp.port_list[(USHORT)vnode->ifindex] || !gMld.port_list[(USHORT)vnode->ifindex]) {
        fprintf(stderr, "harness: setup failed: vlan node %p, ports %u %u\n",
                (void *)vnode, port_idx[0], port_idx[1]);
        return -1;
    }

    /* router port on Ethernet4: an IGMPv3 and an MLDv2 general query */
    memset(q, 0, 12);
    q[0] = 0x11; q[1] = 100; q[8] = 2; q[9] = 125;
    n = mk_ip4(b, q, 12, 0, all_hosts);
    b[12] = 192; b[13] = 0; b[14] = 2; b[15] = 1;               /* 192.0.2.1 */
    fix_igmp_csum(b, n);
    feed_ip4(0, b, n);
    memset(q, 0, 28);
    q[0] = 130; q[4] = 0x27; q[5] = 0x10; q[24] = 2; q[25] = 125;
    n = mk_ip6(b, q, 28, 1, 0, 1);
    b[23] = 0x01;                                               /* fe80::1 */
    fix_mld_csum(b, n);
    feed_ip6(0, b, n);
    if (getenv("HARNESS_DEBUG"))
        fprintf(stderr, "harness: Vlan%d ifindex %u, ports %u %u, tx so far %zu\n",
                VID, vnode->ifindex, port_idx[0], port_idx[1], tx_count);
    return 0;
}

#if defined(FUZZ_IGMP) || defined(FUZZ_MLD)
int LLVMFuzzerInitialize(int *argc, char ***argv)
{
    (void)argc; (void)argv;
    if (setup() < 0)
        abort();
    return 0;
}

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size)
{
    static uint8_t pkt[2048];

    if (size > sizeof(pkt))
        return 0;
    memcpy(pkt, data, size);
#ifdef FUZZ_IGMP
    fix_igmp_csum(pkt, size);
    feed_ip4(1, pkt, size);
#else
    fix_mld_csum(pkt, size);
    feed_ip6(1, pkt, size);
#endif
    return 0;
}
#else
/* ---- regression cases: each must be absorbed with no sanitizer report ---- */
static size_t put16(uint8_t *p, unsigned v) { p[0] = v >> 8; p[1] = v & 0xff; return 2; }

static size_t igmpv3_hdr(uint8_t *b, unsigned num_grps)
{
    memset(b, 0, 8);
    b[0] = 0x22;
    put16(b + 6, num_grps);
    return 8;
}

/* IGMPv3 group record: type, aux words, num_srcs claimed, group 239.1.2.3,
 * then the sources and aux words actually present */
static size_t igmpv3_rec(uint8_t *b, unsigned type, unsigned aux, unsigned claim_srcs,
                         unsigned have_srcs, unsigned have_aux)
{
    size_t n = 8 + 4 * (have_srcs + have_aux);
    unsigned i;

    memset(b, 0, n);
    b[0] = type;
    b[1] = aux;
    put16(b + 2, claim_srcs);
    b[4] = 239; b[5] = 1; b[6] = 2; b[7] = 3;
    for (i = 0; i < have_srcs; i++) {
        b[8 + 4 * i] = 192; b[10 + 4 * i] = 2; b[11 + 4 * i] = 20 + i;
    }
    return n;
}

static size_t mldv2_hdr(uint8_t *b, unsigned num_grps)
{
    memset(b, 0, 8);
    b[0] = 143;
    put16(b + 6, num_grps);
    return 8;
}

/* MLDv2 group record for ff3e::1, sources 2001:db8::N */
static size_t mldv2_rec(uint8_t *b, unsigned type, unsigned aux, unsigned claim_srcs,
                        unsigned have_srcs, unsigned have_aux)
{
    size_t n = 20 + 16 * have_srcs + 4 * have_aux;
    unsigned i;

    memset(b, 0, n);
    b[0] = type;
    b[1] = aux;
    put16(b + 2, claim_srcs);
    b[4] = 0xff; b[5] = 0x3e; b[19] = 0x01;
    for (i = 0; i < have_srcs; i++) {
        b[20 + 16 * i] = 0x20; b[21 + 16 * i] = 0x01; b[22 + 16 * i] = 0x0d; b[23 + 16 * i] = 0xb8;
        b[35 + 16 * i] = 1 + i;
    }
    return n;
}

static void save(const char *name, const uint8_t *pkt, size_t len)
{
    const char *dir = getenv("HARNESS_CORPUS");
    char path[512];
    FILE *f;

    if (!dir)
        return;
    snprintf(path, sizeof(path), "%s/%s/%s", dir, strncmp(name, "mld", 3) ? "igmp" : "mld", name);
    if ((f = fopen(path, "wb"))) {
        fwrite(pkt, 1, len, f);
        fclose(f);
    }
}

/* l2mcd's packet sockets bound to interface ifname (the namespace holds no others) */
static int packet_sockets_on(const char *ifname)
{
    char path[128], line[256];
    unsigned type, proto, iface;
    int ifindex = 0, n = 0;
    FILE *f;

    snprintf(path, sizeof(path), "/sys/class/net/%s/ifindex", ifname);
    if ((f = fopen(path, "r"))) {
        if (fscanf(f, "%d", &ifindex) != 1)
            ifindex = 0;
        fclose(f);
    }
    if (!(f = fopen("/proc/net/packet", "r")))
        return -1;
    while (fgets(line, sizeof(line), f))
        if (sscanf(line, "%*s %*s %u %x %u", &type, &proto, &iface) == 3 && (int)iface == ifindex)
            n++;
    fclose(f);
    return n;
}

/* sockets this process holds */
static int open_sockets(void)
{
    char path[64], target[64];
    int fd, n = 0;
    ssize_t len;

    for (fd = 0; fd < 4096; fd++) {
        snprintf(path, sizeof(path), "/proc/self/fd/%d", fd);
        if ((len = readlink(path, target, sizeof(target) - 1)) > 0) {
            target[len] = 0;
            n += !strncmp(target, "socket:", 7);
        }
    }
    return n;
}

static const char *only;
static int run(const char *name)
{
    if (only && strcmp(only, name))
        return 0;
    fprintf(stderr, "case %s\n", name);
    tx_count = tx_bytes = 0;
    return 1;
}

#define V4(name, claim)  do { size_t t = mk_ip4(pk, m, n, claim, v3_reports); save(name, pk, t); feed_ip4(1, pk, t); } while (0)
#define V4Q(name)        do { size_t t = mk_ip4(pk, m, n, 0, all_hosts); save(name, pk, t); feed_ip4(1, pk, t); } while (0)
#define V6(name, hbh, claim) do { size_t t = mk_ip6(pk, m, n, hbh, claim, 0x16); save(name, pk, t); feed_ip6(1, pk, t); } while (0)

int main(int argc, char **argv)
{
    static uint8_t m[4096], pk[4200];
    size_t n;

    only = argc > 1 ? argv[1] : NULL;
    if (only && !strncmp(only, "query-src-", 10))
        addr_order = strcmp(only, "query-src-addr-after") ? 1 : 2;
    if (setup() < 0)
        return 2;

    /* found on the DX010: queries left from 0.0.0.0 although l2mcd held the
     * VLAN's address (ve_portdb_tree under the VLAN id). Each order of the
     * address and snooping updates, and the live state (ve_ifindex the bare
     * VLAN id, l2mcd_ctl -c 3), must give the address as the source of the
     * group queries (ip_get_lowest_ip_address_on_port) and of a general
     * query. Only when named: the address is set up before the other cases. */
    if (only && (!strcmp(only, "query-src-addr-before") || !strcmp(only, "query-src-addr-after") ||
                 !strcmp(only, "query-src-bare-vid"))) {
        MCGRP_CLASS *igmp = IGMP_GET_INSTANCE_FROM_VRFINDEX(IPVRF_DEFAULT_VRF_IDX);
        UINT16 vir = (USHORT)vnode->ifindex;
        uint32_t lowest;

        run(only);
        if (!strcmp(only, "query-src-bare-vid"))
            vnode->ve_ifindex = VID;
        lowest = ip_get_lowest_ip_address_on_port(vir, MLD_VLAN);
        tx_igmp_src = 0; tx_igmp_type = -1;
        igmp_send_general_query(igmp, vir, PORT_INDEX_INVALID, 3, 0, 100);
        fprintf(stderr, "  Vlan%d ifindex 0x%x ve_ifindex 0x%x: lowest address 0x%08x, general query type 0x%x from 0x%08x (want 0x%08x)\n",
                VID, vnode->ifindex, vnode->ve_ifindex, lowest, tx_igmp_type, tx_igmp_src, VLAN_ADDR_U32);
        if (lowest != VLAN_ADDR_U32 || tx_igmp_type != 0x11 || tx_igmp_src != VLAN_ADDR_U32)
            abort();
        fprintf(stderr, "harness: all cases ran\n");
        return 0;
    }

    /* sanity: well-formed traffic still flows (and is forwarded to the router port) */
    if (run("igmp-valid")) {
        n = igmpv3_hdr(m, 2);
        n += igmpv3_rec(m + n, 1, 0, 1, 1, 0);
        n += igmpv3_rec(m + n, 4, 0, 0, 0, 0);
        V4("igmp-valid", 0);
        fprintf(stderr, "  forwarded %zu packet(s), %zu bytes\n", tx_count, tx_bytes);
    }
    if (run("igmp-valid-aux")) {    /* RFC 3376 aux data: one word, then a second record */
        n = igmpv3_hdr(m, 2);
        n += igmpv3_rec(m + n, 1, 1, 1, 1, 1);
        n += igmpv3_rec(m + n, 4, 0, 0, 0, 0);
        V4("igmp-valid-aux", 0);
    }
    /* finding 1: the record walk trusts num_srcs, num_grps and aux_data_len */
    if (run("igmp-65535-sources")) {
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 1, 0, 65535, 1, 0);
        V4("igmp-65535-sources", 0);
    }
    if (run("igmp-65535-groups")) {
        n = igmpv3_hdr(m, 65535);
        n += igmpv3_rec(m + n, 1, 0, 0, 0, 0);
        V4("igmp-65535-groups", 0);
    }
    if (run("igmp-truncated-record")) {
        n = igmpv3_hdr(m, 2);
        n += igmpv3_rec(m + n, 1, 0, 1, 1, 0);
        m[n] = 1; m[n + 1] = 0; m[n + 2] = 0;
        n += 3;
        V4("igmp-truncated-record", 0);
    }
    if (run("igmp-aux-claimed")) {  /* 200 aux words claimed, none present */
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 1, 200, 0, 0, 0);
        V4("igmp-aux-claimed", 0);
    }
    /* finding 2: record types outside 1..6 index the stats and label arrays */
    if (run("igmp-type-0")) {
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 0, 0, 0, 0, 0);
        V4("igmp-type-0", 0);
    }
    if (run("igmp-type-200")) {
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 200, 0, 1, 1, 0);
        V4("igmp-type-200", 0);
    }
    /* finding 5: tot_len is trusted (and checksum 0 skips the check) */
    if (run("igmp-totlen-beyond-packet")) {
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 1, 0, 0, 0, 0);
        V4("igmp-totlen-beyond-packet", 1500);
    }
    if (run("igmp-totlen-forwarded")) { /* checksum 0, tot_len 300: forwarded as 300 bytes */
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 4, 0, 0, 0, 0);
        {
            size_t t = mk_ip4(pk, m, n, 300, v3_reports);
            pk[24 + 2] = pk[24 + 3] = 0;
            save("igmp-totlen-forwarded", pk, t);
            feed_ip4(1, pk, t);
        }
    }
    if (run("igmp-ihl-beyond-totlen")) {
        n = igmpv3_hdr(m, 1);
        n += igmpv3_rec(m + n, 1, 0, 0, 0, 0);
        {
            size_t t = mk_ip4(pk, m, n, 20, v3_reports);
            save("igmp-ihl-beyond-totlen", pk, t);
            feed_ip4(1, pk, t);
        }
    }
    if (run("igmp-short-message")) {    /* 4 bytes of IGMP: group read past the end */
        m[0] = 0x16; m[1] = 0; m[2] = 0; m[3] = 0;
        n = 4;
        V4("igmp-short-message", 0);
    }
    /* finding 6: query source lists are read with num_srcs in network order */
    if (run("igmp-v3-query-sources")) {
        memset(m, 0, 16);
        m[0] = 0x11; m[1] = 100;
        put16(m + 10, 1);
        m[12] = 192; m[14] = 2; m[15] = 30;
        n = 16;
        V4Q("igmp-v3-query-sources");
    }
    if (run("igmp-v1-query-group")) {  /* found by fuzzing the fixed tree: 8-byte v1 query, group set */
        memset(m, 0, 8);
        m[0] = 0x11; m[1] = 0;
        m[4] = 239; m[5] = 1; m[6] = 2; m[7] = 3;
        n = 8;
        V4Q("igmp-v1-query-group");
    }
    if (run("port-list-repeated")) { /* found on a VS: every port update opened another IGMP socket */
        int before = open_sockets(), after, i;
        l2mcd_if_tree_t *t = l2mcd_if_to_kif(port_idx[1]);
        fprintf(stderr, "  Ethernet8: if_tree %p igmp fd %d mld fd %d, /proc/net/packet %d\n", (void *)t,
                t ? t->igmp_sock_fd : -1, t ? t->mld_sock_fd : -1, packet_sockets_on("Ethernet8"));
        for (i = 0; i < 3; i++)
            cfg(L2MCD_SNOOP_PORT_LIST_MSG, L2MCD_IPV4_AFI, 0, 1, 2);
        after = open_sockets();
        t = l2mcd_if_to_kif(port_idx[1]);
        fprintf(stderr, "  Ethernet8: if_tree %p igmp fd %d mld fd %d\n", (void *)t, t ? t->igmp_sock_fd : -1, t ? t->mld_sock_fd : -1);
        fprintf(stderr, "  open sockets: %d, then %d after three more port lists\n", before, after);
        if (after != before)
            abort();
    }
    if (run("mld-valid")) {
        n = mldv2_hdr(m, 3);
        n += mldv2_rec(m + n, 1, 0, 1, 1, 0);
        n += mldv2_rec(m + n, 4, 0, 0, 0, 0);
        n += mldv2_rec(m + n, 1, 0, 2, 2, 0);
        V6("mld-valid", 1, 0);
        fprintf(stderr, "  forwarded %zu packet(s), %zu bytes\n", tx_count, tx_bytes);
    }
    /* finding 3: the MLDv2 walk (cumulative offset, aux ignored, no bounds) */
    if (run("mld-65535-sources")) {
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 1, 0, 65535, 1, 0);
        V6("mld-65535-sources", 1, 0);
    }
    if (run("mld-65535-groups")) {
        n = mldv2_hdr(m, 65535);
        n += mldv2_rec(m + n, 1, 0, 0, 0, 0);
        V6("mld-65535-groups", 1, 0);
    }
    if (run("mld-zero-sources-with-aux")) {
        n = mldv2_hdr(m, 2);
        n += mldv2_rec(m + n, 4, 2, 0, 0, 2);
        n += mldv2_rec(m + n, 4, 0, 0, 0, 0);
        V6("mld-zero-sources-with-aux", 1, 0);
    }
    if (run("mld-type-0")) {
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 0, 0, 0, 0, 0);
        V6("mld-type-0", 1, 0);
    }
    if (run("mld-type-200")) {
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 200, 0, 1, 1, 0);
        V6("mld-type-200", 1, 0);
    }
    /* finding 4: receive_mld_packet trusts the header chain and drops nothing */
    if (run("mld-no-hbh")) {        /* next header ICMPv6: hbh_len never set */
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 4, 0, 0, 0, 0);
        V6("mld-no-hbh", 0, 0);
    }
    if (run("mld-hbh-oversized")) { /* hop-by-hop claims 2048 bytes */
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 4, 0, 0, 0, 0);
        {
            size_t t = mk_ip6(pk, m, n, 1, 0, 0x16);
            pk[41] = 255;
            save("mld-hbh-oversized", pk, t);
            feed_ip6(1, pk, t);
        }
    }
    if (run("mld-payload-beyond-packet")) {  /* a nonzero checksum, so it is computed */
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 4, 0, 0, 0, 0);
        {
            size_t t = mk_ip6(pk, m, n, 1, 1400, 0x16);
            pk[48 + 2] = 0x12; pk[48 + 3] = 0x34;
            save("mld-payload-beyond-packet", pk, t);
            feed_ip6(1, pk, t);
        }
    }
    if (run("mld-v2-query-sources")) {  /* finding 6 for MLD: needs a membership first */
        n = mldv2_hdr(m, 1);
        n += mldv2_rec(m + n, 1, 0, 1, 1, 0);
        V6("mld-v2-query-sources-join", 1, 0);
        memset(m, 0, 44);
        m[0] = 130; m[4] = 0x03; m[5] = 0xe8;
        m[8] = 0xff; m[9] = 0x3e; m[23] = 0x01;                 /* ff3e::1 */
        put16(m + 26, 1);
        m[28] = 0x20; m[29] = 0x01; m[30] = 0x0d; m[31] = 0xb8; m[43] = 1;
        n = 44;
        {
            size_t t = mk_ip6(pk, m, n, 1, 0, 0);
            pk[24] = 0xff; pk[25] = 0x3e; pk[39] = 0x01;        /* to ff3e::1 */
            fix_mld_csum(pk, t);
            save("mld-v2-query-sources", pk, t);
            feed_ip6(1, pk, t);
        }
    }
#ifdef L2MCD_RX_DROP_COUNTERS
    fprintf(stderr, "harness: dropped as malformed: igmp %u, mld %u; forwarded %zu packet(s)\n",
            g_rx_stats_malformed_igmp, g_rx_stats_malformed_mld, tx_count);
#endif
    fprintf(stderr, "harness: all cases ran\n");
    return 0;
}
#endif
