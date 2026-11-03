/* stubs.c -- the C side of l2mcd_sync.cpp (APPL_DB writes over swss-common),
 * replaced by no-ops so the harness runs without Redis. */
#include <stdint.h>
#include <stdarg.h>
#include "l2mcd.h"

void l2mcsync_add_vlan_entry(uint16_t vlan_id) { (void)vlan_id; }
int l2mcdsync_get_l2mcmgr_debug_prio(void) { return 0; }
void l2mcsync_del_vlan_entry(uint16_t vlan_id) { (void)vlan_id; }
void l2mcsync_add_l2mc_entry(L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
void l2mcsync_del_l2mc_entry(L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
void l2mcsync_process_mrouterentry(L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
void l2mcsync_debug_print(const char *fmt, ...) { (void)fmt; }
int l2mcd_is_peerlink(char *portname) { (void)portname; return 0; }
void l2mcsync_notify_config_done(char *option, char *paraname) { (void)option; (void)paraname; }
void l2mcsync_notify_warm_reboot_done(char *option, char *paraname) { (void)option; (void)paraname; }
void l2mcsync_clear_l2mc_entry(void) { }
int l2mcsync_get_l2mc_info_count(uint16_t vlan_id, uint16_t afi) { (void)vlan_id; (void)afi; return 0; }
void l2mcsync_dump_l2mc_info(DUMP_L2MCD_APP_TABLE_ENTRY *msg) { (void)msg; }
