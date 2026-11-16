/* stubs.c -- the C side of l2mcd_sync.cpp (APPL_DB writes over swss-common),
 * replaced by no-ops so the harness runs without Redis. */
#include <stdint.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <sanitizer/common_interface_defs.h>
#include "l2mcd.h"

void l2mcsync_add_vlan_entry(uint16_t vlan_id) { (void)vlan_id; }
int l2mcdsync_get_l2mcmgr_debug_prio(void) { return 0; }
void l2mcsync_del_vlan_entry(uint16_t vlan_id) { (void)vlan_id; }
/* the group-member deletions l2mcd would write to APPL_DB, counted for the
 * port-list-breakout and member-* cases. l2mcd sends them through
 * l2mcsync_add_l2mc_entry() with op_code 0 (l2mcd_system_group_entry_notify();
 * L2mcSync::addL2mcTableEntry() deletes the APPL_DB row for op_code 0); no C
 * file calls l2mcsync_del_l2mc_entry(). Until 2026-11-15 only the second was
 * counted, so no case could see a deletion. */
unsigned stub_l2mc_dels;
char stub_l2mc_del_port[L2MCD_IFNAME_SIZE];
static void stub_count_del(const L2MCD_APP_TABLE_ENTRY *msg)
{
    stub_l2mc_dels++;
    snprintf(stub_l2mc_del_port, sizeof(stub_l2mc_del_port), "%s", msg->port.pnames);
}
void l2mcsync_add_l2mc_entry(L2MCD_APP_TABLE_ENTRY *msg)
{
    if (!msg->op_code)
        stub_count_del(msg);
}
void l2mcsync_del_l2mc_entry(L2MCD_APP_TABLE_ENTRY *msg) { stub_count_del(msg); }
void l2mcsync_process_mrouterentry(L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
void l2mcsync_debug_print(const char *fmt, ...) { (void)fmt; }
/* l2mcd_sync.cpp makes a std::string of the name (L2mcSync::isPortPeerLink
 * takes one): from NULL that throws std::logic_error, which nothing in l2mcd
 * catches, so the daemon aborts (the DX010, 2026-11-15). The stub does the same,
 * with the caller's stack. */
int l2mcd_is_peerlink(char *portname)
{
    if (!portname) {
        fprintf(stderr, "l2mcd_is_peerlink(NULL): terminate called after throwing an instance of "
                "'std::logic_error' (basic_string: construction from null is not valid)\n");
        __sanitizer_print_stack_trace();
        abort();
    }
    return 0;
}
void l2mcsync_notify_config_done(char *option, char *paraname) { (void)option; (void)paraname; }
void l2mcsync_notify_warm_reboot_done(char *option, char *paraname) { (void)option; (void)paraname; }
void l2mcsync_clear_l2mc_entry(void) { }
int l2mcsync_get_l2mc_info_count(uint16_t vlan_id, uint16_t afi) { (void)vlan_id; (void)afi; return 0; }
void l2mcsync_dump_l2mc_info(DUMP_L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
