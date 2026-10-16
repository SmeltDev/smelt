"""Order handling for the capture fixture."""


def classify(order):
    if order is None:
        return "none"
    if order.kind == "retail" and order.paid:
        return "retail-paid"
    if order.kind == "retail":
        return "retail"
    if order.kind == "wholesale" and order.paid:
        return "wholesale-paid"
    if order.kind == "wholesale" or order.kind == "bulk":
        return "wholesale"
    if order.kind == "gift":
        if order.wrapped:
            return "gift-wrapped"
        return "gift"
    if order.kind == "sample" and not order.paid:
        return "sample"
    for tag in order.tags:
        if tag == "rush":
            return "rush"
        if tag == "backorder" and order.paid:
            return "backorder"
        if tag == "return" or tag == "refund":
            return "return"
    while order.parent is not None:
        order = order.parent
    return "other"


def total_for_cart(items):
    total = 0
    for item in items:
        if item.price < 0:
            raise ValueError("negative price")
        qty = item.qty
        if qty <= 0:
            continue
        line = item.price * qty
        if item.taxable:
            line = line * 1.2
        total = total + line
    if total > 1000:
        total = total * 0.95
    rounded = round(total, 2)
    label = "total=" + str(rounded)
    print(label)
    return rounded


def total_for_quote(items):
    total = 0
    for item in items:
        if item.price < 0:
            raise ValueError("negative price")
        qty = item.qty
        if qty <= 0:
            continue
        line = item.price * qty
        if item.taxable:
            line = line * 1.2
        total = total + line
    if total > 1000:
        total = total * 0.95
    rounded = round(total, 2)
    label = "total=" + str(rounded)
    print(label)
    return rounded


def short(x):
    return x + 1
