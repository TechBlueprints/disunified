# The kernel bridge's filter table for the l2d fakes (l2d_dot1x.py,
# l2d_mab.py, l2d_mab_restart.py, l2d_maclock.py): ebtables' single
# commands, each its own transaction, and ebtables-restore --noflush as the
# DX010's ebtables (1.8.11, nf_tables) runs it (docs/drivers/sonic.md
# 11.26.12, measured there): the whole input is one transaction, a declared
# chain (":<chain> <policy>") is created or emptied, and a refused line
# refuses the batch and leaves the table as it was.
#
# verdict() walks a frame through INPUT as the kernel does, and watch()
# wraps a guard's write_chain: a port that dropped a never-admitted source
# before and after the call must drop it at every transaction in between,
# or the fake prints "WINDOW <guard> <port> <transactions that passed it>".
import copy

PROBE = "02:00:00:00:0f:ff"  # a source no rig admits


class Refused(Exception):
    pass


class Filter(object):
    def __init__(self, chains, user_policy="RETURN"):
        self.chains = chains  # chain -> list of rules (token lists); the fakes' CHAINS
        self.user_policy = user_policy
        self.after = []  # called after each transaction that changed something
        self.states = None  # while watched: the closed ports after each transaction

    def policy(self, chain):
        return "ACCEPT" if chain in ("INPUT", "FORWARD", "OUTPUT") else self.user_policy

    def listing(self):
        out = ["Bridge table: filter", ""]
        for c, rules in self.chains.items():
            out.append("Bridge chain: %s, entries: %d, policy: %s" % (c, len(rules), self.policy(c)))
            out += [" ".join(r) for r in rules]
            out.append("")
        return "\n".join(out) + "\n"

    @staticmethod
    def apply(chains, args):
        op, chain, rule = args[0], args[1], args[2:]
        if op == "-N":
            if chain in chains:
                raise Refused("chain %s exists" % chain)
            chains[chain] = []
            return
        if chain not in chains:
            raise Refused("no chain %s" % chain)
        if op == "-X":
            if chains[chain] or any(r[-2:] == ["-j", chain] for rs in chains.values() for r in rs):
                raise Refused("chain %s in use" % chain)
            del chains[chain]
        elif op == "-F":
            chains[chain] = []
        elif op == "-A":
            chains[chain].append(rule)
        elif op == "-I":
            chains[chain].insert(int(rule[0]) - 1, rule[1:])
        elif op == "-D":
            if rule not in chains[chain]:
                raise Refused("no such rule")
            chains[chain].remove(rule)
        else:
            raise Refused("op " + op)
        target = rule[-1] if rule[-2:-1] == ["-j"] else ""
        if target and target not in ("ACCEPT", "DROP", "RETURN", "CONTINUE") and target not in chains:
            raise Refused("no chain %s" % target)

    def committed(self):
        for f in self.after:
            f()
        if self.states is not None:
            self.states.append(self.closed())

    def ebtables(self, args):
        """One ebtables command: (stdout, exit status)."""
        if args == ["-L"]:
            return self.listing(), 0
        try:
            self.apply(self.chains, args)
        except Refused:
            return "", 1
        self.committed()
        return "", 0

    def restore(self, text):
        """ebtables-restore --noflush: (printed lines, exit status)."""
        work = copy.deepcopy(self.chains)
        lines = [l for l in text.splitlines() if l.strip()]
        try:
            if not lines or lines[0] != "*filter":
                raise Refused("no table")
            for l in lines[1:]:
                if l == "COMMIT":
                    continue
                if l.startswith(":"):
                    work[l[1:].split()[0]] = []
                    continue
                self.apply(work, l.split())
        except Refused:
            return lines, 2
        self.chains.clear()
        self.chains.update(work)
        self.committed()
        return lines, 0

    # --- the kernel's walk ---------------------------------------------------

    @staticmethod
    def matches(rule, port, src):
        i = 0
        while i < len(rule) and rule[i] != "-j":
            k, v = rule[i], rule[i + 1]
            if k == "-i" and v != port:
                return False
            if k == "-s" and v != src:
                return False
            if k == "-p":  # the probe is an untagged 0x88b5 frame
                return False
            i += 2
        return True

    def walk(self, chain, port, src, depth=0):
        for r in self.chains.get(chain, []):
            if not self.matches(r, port, src):
                continue
            t = r[-1]
            if t in ("DROP", "ACCEPT"):
                return t
            if t == "RETURN":
                return None
            if t == "CONTINUE":
                continue
            v = self.walk(t, port, src, depth + 1)
            if v:
                return v
        return None

    def verdict(self, port, src=PROBE):
        return self.walk("INPUT", port, src) or "ACCEPT"

    def ports(self):
        return {r[r.index("-i") + 1] for rs in self.chains.values() for r in rs if "-i" in r}

    def closed(self):
        return {p for p in self.ports() if self.verdict(p) == "DROP"}

    def watch(self, cls, name):
        """Wraps cls.write_chain (a guard's) with the window check."""
        real = cls.write_chain
        eb = self

        def write_chain(guard, rules):
            before = eb.closed()
            eb.states = []
            try:
                return real(guard, rules)
            finally:
                states, eb.states = eb.states, None
                after = eb.closed()
                for p in sorted(before & after):
                    n = sum(1 for s in states if p not in s)
                    if n:
                        print("WINDOW %s %s %d" % (name, p, n))

        cls.write_chain = write_chain
