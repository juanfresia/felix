// Project Calico BPF dataplane programs.
// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.
//
// This program is free software; you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 2 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License along
// with this program; if not, write to the Free Software Foundation, Inc.,
// 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301 USA.

#ifndef __CALI_PARSING_H__
#define __CALI_PARSING_H__

#define PARSING_OK 0
#define PARSING_ERROR -1
#define PARSING_ALLOW_WITHOUT_ENFORCING_POLICY -2

static CALI_BPF_INLINE int parse_packet_ip(struct cali_tc_ctx *ctx) {
	__u16 protocol = 0;

	/* We need to make a decision based on Ethernet protocol, however,
	 * the protocol number is not available to XDP programs like TC ones.
	 * In TC programs protocol number is available via skb->protocol.
	 * For that, in XDP programs we need to parse at least up to Ethernet
	 * first, before making any decision. But in TC programs we can make
	 * an initial decision based on Ethernet protocol before parsing packet
	 * for more headers.
	 */
	if (CALI_F_XDP) {
		if (skb_refresh_validate_ptrs(ctx, UDP_SIZE)) {
			ctx->fwd.reason = CALI_REASON_SHORT;
			CALI_DEBUG("Too short\n");
			goto deny;
		}
		protocol = bpf_ntohs(ctx->eth->h_proto);
	} else {
		protocol = bpf_ntohs(ctx->skb->protocol);
	}

	switch (protocol) {
	case ETH_P_IP:
		break;
	case ETH_P_ARP:
		CALI_DEBUG("ARP: allowing packet\n");
		goto allow_no_fib;
	case ETH_P_IPV6:
		if (CALI_F_WEP) {
			CALI_DEBUG("IPv6 from workload: drop\n");
			goto deny;
		} else {
			// FIXME: support IPv6.
			CALI_DEBUG("IPv6 on host interface: allow\n");
			goto allow_no_fib;
		}
	default:
		if (CALI_F_WEP) {
			CALI_DEBUG("Unknown ethertype (%x), drop\n", protocol);
			goto deny;
		} else {
			CALI_DEBUG("Unknown ethertype on host interface (%x), allow\n",
									protocol);
			goto allow_no_fib;
		}
	}

	// In TC programs, parse packet and validate its size. This is
	// already done for XDP programs at the beginning of the function.
	if (!CALI_F_XDP) {
		if (skb_refresh_validate_ptrs(ctx, UDP_SIZE)) {
			ctx->fwd.reason = CALI_REASON_SHORT;
			CALI_DEBUG("Too short\n");
			goto deny;
		}
	}

	// Drop malformed IP packets
	if (ctx->ip_header->ihl < 5) {
		ctx->fwd.reason = CALI_REASON_IP_MALFORMED;
		CALI_DEBUG("Drop malformed IP packets\n");
		goto deny;
	} else if (ctx->ip_header->ihl > 5) {
		/* Drop packets with IP options from/to WEP.
		 * Also drop packets with IP options if the dest IP is not host IP
		 */
		if (CALI_F_WEP || (CALI_F_FROM_HEP && !rt_addr_is_local_host(ctx->ip_header->daddr))) {
			ctx->fwd.reason = CALI_REASON_IP_OPTIONS;
			CALI_DEBUG("Drop packets with IP options\n");
			goto deny;
		}
		CALI_DEBUG("Allow packets with IP options and dst IP = hostIP\n");
		goto allow_no_fib;
	}

	return PARSING_OK;

allow_no_fib:
	return PARSING_ALLOW_WITHOUT_ENFORCING_POLICY;

deny:
	return PARSING_ERROR;
}

static CALI_BPF_INLINE void tc_state_fill_from_iphdr(struct cali_tc_ctx *ctx)
{
	ctx->state->ip_src = ctx->ip_header->saddr;
	ctx->state->ip_dst = ctx->ip_header->daddr;
	ctx->state->pre_nat_ip_dst = ctx->ip_header->daddr;
	ctx->state->ip_proto = ctx->ip_header->protocol;
	ctx->state->ip_size = ctx->ip_header->tot_len;
}

/* Continue parsing packet based on the IP protocol and fill in relevant fields
 * in the state (struct cali_tc_state). */
static CALI_BPF_INLINE int tc_state_fill_from_nexthdr(struct cali_tc_ctx *ctx)
{
	switch (ctx->state->ip_proto) {
	case IPPROTO_TCP:
		// Re-check buffer space for TCP (has larger headers than UDP).
		if (skb_refresh_validate_ptrs(ctx, TCP_SIZE)) {
			ctx->fwd.reason = CALI_REASON_SHORT;
			CALI_DEBUG("Too short\n");
			goto deny;
		}
		ctx->state->sport = bpf_ntohs(ctx->tcp_header->source);
		ctx->state->dport = bpf_ntohs(ctx->tcp_header->dest);
		ctx->state->pre_nat_dport = ctx->state->dport;
		CALI_DEBUG("TCP; ports: s=%d d=%d\n", ctx->state->sport, ctx->state->dport);
		break;
	case IPPROTO_UDP:
		ctx->state->sport = bpf_ntohs(ctx->udp_header->source);
		ctx->state->dport = bpf_ntohs(ctx->udp_header->dest);
		ctx->state->pre_nat_dport = ctx->state->dport;
		CALI_DEBUG("UDP; ports: s=%d d=%d\n", ctx->state->sport, ctx->state->dport);
		if (ctx->state->dport == VXLAN_PORT) {
			/* CALI_F_FROM_HEP case is handled in vxlan_attempt_decap above since it already decoded
			 * the header. */
			if (CALI_F_TO_HEP) {
				if (rt_addr_is_remote_host(ctx->state->ip_dst) &&
						rt_addr_is_local_host(ctx->state->ip_src)) {
					CALI_DEBUG("VXLAN packet to known Calico host, allow.\n");
					goto allow;
				} else {
					/* Unlike IPIP, the user can be using VXLAN on a different VNI
					 * so we don't simply drop it. */
					CALI_DEBUG("VXLAN packet to unknown dest, fall through to policy.\n");
				}
			}
		}
		break;
	case IPPROTO_ICMP:
		ctx->state->icmp_type = ctx->icmp_header->type;
		ctx->state->icmp_code = ctx->icmp_header->code;
		CALI_DEBUG("ICMP; type=%d code=%d\n",
				ctx->icmp_header->type, ctx->icmp_header->code);
		break;
	case IPPROTO_IPIP:
		if (CALI_F_TUNNEL | CALI_F_WIREGUARD) {
			// IPIP should never be sent down the tunnel.
			CALI_DEBUG("IPIP traffic to/from tunnel: drop\n");
			ctx->fwd.reason = CALI_REASON_UNAUTH_SOURCE;
			goto deny;
		}
		if (CALI_F_FROM_HEP) {
			if (rt_addr_is_remote_host(ctx->state->ip_src)) {
				CALI_DEBUG("IPIP packet from known Calico host, allow.\n");
				goto allow;
			} else {
				CALI_DEBUG("IPIP packet from unknown source, drop.\n");
				ctx->fwd.reason = CALI_REASON_UNAUTH_SOURCE;
				goto deny;
			}
		} else if (CALI_F_TO_HEP && !CALI_F_TUNNEL && !CALI_F_WIREGUARD) {
			if (rt_addr_is_remote_host(ctx->state->ip_dst)) {
				CALI_DEBUG("IPIP packet to known Calico host, allow.\n");
				goto allow;
			} else {
				CALI_DEBUG("IPIP packet to unknown dest, drop.\n");
				ctx->fwd.reason = CALI_REASON_UNAUTH_SOURCE;
				goto deny;
			}
		}
		if (CALI_F_FROM_WEP) {
			CALI_DEBUG("IPIP traffic from workload: drop\n");
			ctx->fwd.reason = CALI_REASON_UNAUTH_SOURCE;
			goto deny;
		}
	default:
		CALI_DEBUG("Unknown protocol (%d), unable to extract ports\n",
					(int)ctx->state->ip_proto);
	}

	return PARSING_OK;

allow:
	return PARSING_ALLOW_WITHOUT_ENFORCING_POLICY;

deny:
	return PARSING_ERROR;
}

#endif /* __CALI_PARSING_H__ */
